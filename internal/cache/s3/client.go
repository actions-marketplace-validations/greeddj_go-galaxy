package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Client implements minimal S3 operations with SigV4 signing.
type Client struct {
	client *http.Client
	cfg    config.S3CacheConfig
}

// newClient constructs an S3 client from configuration.
func newClient(cfg config.S3CacheConfig, httpClient *http.Client) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, errS3BucketEmpty
	}
	if httpClient == nil {
		return nil, errS3HTTPClientNil
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", cfg.Region)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: %s", errS3InvalidEndpoint, endpoint)
	}
	cfg.Endpoint = strings.TrimRight(endpoint, "/")
	return &Client{cfg: cfg, client: httpClient}, nil
}

// s3ErrorResponse captures the fields S3 puts in the XML <Error> document
// that many non-2xx responses carry in their body, alongside the HTTP status
// line.
type s3ErrorResponse struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// s3StatusError builds an error wrapping sentinel that folds in the response
// body's S3 error code and message when one is present, falling back to the
// bare HTTP status line when the body is empty, is not XML, or lacks a Code
// element. Reading or parsing the body never fails this call outright - a
// malformed or truncated error body must not hide the original failure - and
// the caller remains responsible for closing resp.Body afterward.
func s3StatusError(sentinel error, resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, s3ErrorBodyLimit))
	if err != nil {
		return fmt.Errorf("%w: %s", sentinel, resp.Status)
	}
	var parsed s3ErrorResponse
	if err := xml.Unmarshal(data, &parsed); err != nil || parsed.Code == "" {
		return fmt.Errorf("%w: %s", sentinel, resp.Status)
	}
	return fmt.Errorf("%w: %s (%s: %s)", sentinel, resp.Status, parsed.Code, parsed.Message)
}

// getObject performs a GET request for the object key, retrying a
// transient failure (a retryable HTTP status or a stalled body read) up to
// s3RetryPolicy's bound. Each attempt builds a fresh request via newRequest
// so its X-Amz-Date and signature are never stale by the time a retry
// fires; a failed attempt's response body is closed before the next one, and
// only the eventual successful response is returned open for the caller to
// read and close.
func (c *Client) getObject(ctx context.Context, key string) (*http.Response, error) {
	var success *http.Response
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodGet, key, nil, nil, emptySHA256, nil, false)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return errS3NotFound
		}
		if resp.StatusCode != http.StatusOK {
			statusErr := wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3GetFailed, resp))
			_ = resp.Body.Close()
			return statusErr
		}
		success = resp
		return nil
	}, s3Retryable)
	if err != nil {
		return nil, err
	}
	return success, nil
}

// headObject performs a HEAD request for the object key, retrying a
// transient failure like getObject. HEAD responses never carry a body
// (net/http elides it even if a handler writes one), so the failure here
// stays status-only rather than going through s3StatusError.
func (c *Client) headObject(ctx context.Context, key string) (http.Header, error) {
	var headers http.Header
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodHead, key, nil, nil, emptySHA256, nil, false)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return errS3NotFound
		}
		if resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, fmt.Errorf("%w: %s", errS3HeadFailed, resp.Status))
		}
		headers = resp.Header.Clone()
		return nil
	}, s3Retryable)
	if err != nil {
		return nil, err
	}
	return headers, nil
}

// putObject uploads an object with optional metadata. A conditional
// create-if-absent PUT (ifNoneMatch) is single-shot and never retried: a
// lost-success retry would observe 412 (the object it just created now
// exists) and misreport its own success as contention, which the
// distributed lock's acquireLock loop cannot distinguish from a live
// holder - see reclaimIfExpired/tryAcquireOnce, whose own loop is the sole
// retrier of conditional PUTs. An unconditional overwrite is safe to retry:
// each attempt reseeks body to its start and rebuilds the request (fresh
// signature) before resending.
func (c *Client) putObject(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	contentType, contentEncoding string,
	meta map[string]string,
	ifNoneMatch bool,
	payloadHash string,
) error {
	payloadHash, err := resolvePayloadHash(body, payloadHash)
	if err != nil {
		return err
	}
	attempt := func() error {
		req, err := c.newRequest(ctx, http.MethodPut, key, nil, body, payloadHash, meta, ifNoneMatch)
		if err != nil {
			return err
		}
		req.ContentLength = size
		applyContentHeaders(req, contentType, contentEncoding)
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		return handlePutResponse(resp)
	}
	if ifNoneMatch {
		return attempt()
	}
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return attempt()
	}, s3Retryable)
}

// deleteObject deletes an object by key, retrying a transient failure like
// getObject. A missing object (404) is treated as already deleted, matching
// S3's own idempotent DELETE semantics.
func (c *Client) deleteObject(ctx context.Context, key string) error {
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodDelete, key, nil, nil, emptySHA256, nil, false)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3DeleteFailed, resp))
		}
		return nil
	}, s3Retryable)
}

// listObjects returns object keys under the given prefix.
func (c *Client) listObjects(ctx context.Context, prefix string) ([]string, error) {
	keys := []string{}
	var token string
	for {
		result, err := c.listObjectsPage(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		keys = appendKeys(keys, result.Contents)
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	return keys, nil
}

func resolvePayloadHash(body io.ReadSeeker, payloadHash string) (string, error) {
	if payloadHash != "" {
		return payloadHash, nil
	}
	hash, err := hashReader(body)
	if err != nil {
		return "", err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hash, nil
}

func applyContentHeaders(req *http.Request, contentType, contentEncoding string) {
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
}

func handlePutResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusPreconditionFailed:
		return errS3PreconditionFailed
	case http.StatusNotFound:
		return errS3BucketNotFound
	case http.StatusOK, http.StatusNoContent:
		return nil
	default:
		return wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3PutFailed, resp))
	}
}

// listObjectsPage fetches one ListObjectsV2 page, retrying the whole
// request-read-parse cycle on a transient failure. bucketRequest is
// single-shot, so this outer retry is the sole layer: it recovers both a
// transient status (surfaced by bucketRequest as a retryable-classified
// error) and a body-read stall (helpers.ErrReadStalled) during the io.ReadAll
// here, which happens after bucketRequest has already returned a 200, within
// one bounded attempt budget.
func (c *Client) listObjectsPage(ctx context.Context, prefix, token string) (listBucketResult, error) {
	query := url.Values{}
	query.Set("list-type", "2")
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	var result listBucketResult
	err := helpers.Retry(ctx, s3RetryPolicy(), func() error {
		resp, err := c.bucketRequest(ctx, http.MethodGet, query)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, helpers.S3ListMaxSize))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		var parsed listBucketResult
		if err := xml.Unmarshal(data, &parsed); err != nil {
			return err
		}
		result = parsed
		return nil
	}, s3Retryable)
	if err != nil {
		return listBucketResult{}, err
	}
	return result, nil
}

func appendKeys(dst []string, contents []listBucketContent) []string {
	for _, item := range contents {
		if item.Key != "" {
			dst = append(dst, item.Key)
		}
	}
	return dst
}

// ensureBucket creates the bucket when it does not exist.
func (c *Client) ensureBucket(ctx context.Context) error {
	if err := c.headBucket(ctx); err != nil {
		if errors.Is(err, errS3BucketNotFound) {
			return c.createBucket(ctx)
		}
		return err
	}
	return nil
}

// headBucket checks whether the configured bucket exists. Like headObject,
// this is a HEAD request with no response body, so its failure stays
// status-only rather than going through s3StatusError. It retries a
// transient failure like the other idempotent verbs.
func (c *Client) headBucket(ctx context.Context) error {
	return helpers.Retry(ctx, s3RetryPolicy(), func() error {
		req, err := c.newRequest(ctx, http.MethodHead, "", nil, nil, emptySHA256, nil, false)
		if err != nil {
			return err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode == http.StatusNotFound {
			return errS3BucketNotFound
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return wrapRetryableStatus(resp.StatusCode, fmt.Errorf("%w: %s", errS3BucketHeadFailed, resp.Status))
		}
		return nil
	}, s3Retryable)
}

// createBucket sends a CreateBucket request with region configuration.
func (c *Client) createBucket(ctx context.Context) error {
	var (
		body        io.ReadSeeker
		contentType string
		contentSize int64
		payloadHash = emptySHA256
	)
	if c.cfg.Region != "" && c.cfg.Region != "us-east-1" {
		payload := fmt.Appendf(nil,
			"<CreateBucketConfiguration xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\">"+
				"<LocationConstraint>%s</LocationConstraint>"+
				"</CreateBucketConfiguration>",
			c.cfg.Region,
		)
		hash := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(hash[:])
		body = bytes.NewReader(payload)
		contentType = "application/xml"
		contentSize = int64(len(payload))
	}
	req, err := c.newRequest(ctx, http.MethodPut, "", nil, body, payloadHash, nil, false)
	if err != nil {
		return err
	}
	req.ContentLength = contentSize
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return s3StatusError(errS3CreateBucketFailed, resp)
	}
	return nil
}

// bucketRequest issues a single request against the bucket root. Its sole
// caller, listObjectsPage, owns the retry, so a transient status (returned
// here as a retryable-classified error via wrapRetryableStatus) and a
// body-read stall during the listing share one bounded attempt budget rather
// than nesting two. On a non-2xx it closes the response body and returns; on
// 200 it returns the response with its body still open for the caller to read.
func (c *Client) bucketRequest(ctx context.Context, method string, query url.Values) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, "", query, nil, emptySHA256, nil, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, errS3BucketNotFound
	}
	if resp.StatusCode != http.StatusOK {
		statusErr := wrapRetryableStatus(resp.StatusCode, s3StatusError(errS3BucketRequestFailed, resp))
		_ = resp.Body.Close()
		return nil, statusErr
	}
	return resp, nil
}

// listBucketResult represents the S3 ListBucket XML response.
type listBucketResult struct {
	NextContinuationToken  string              `xml:"NextContinuationToken"`
	ContinuationToken      string              `xml:"ContinuationToken"`
	Prefix                 string              `xml:"Prefix"`
	Delimiter              string              `xml:"Delimiter"`
	StartAfter             string              `xml:"StartAfter"`
	ContinuationTokenStart string              `xml:"ContinuationTokenStart"`
	Contents               []listBucketContent `xml:"Contents"`
	CommonPrefixes         []listBucketPrefix  `xml:"CommonPrefixes"`
	KeyCount               int                 `xml:"KeyCount"`
	MaxKeys                int                 `xml:"MaxKeys"`
	IsTruncated            bool                `xml:"IsTruncated"`
}

// listBucketContent represents an object entry in a ListBucket response.
type listBucketContent struct {
	Key string `xml:"Key"`
}

// listBucketPrefix represents a common prefix entry in a ListBucket response.
type listBucketPrefix struct {
	Prefix string `xml:"Prefix"`
}

// newRequest builds and signs a request for the given object key.
func (c *Client) newRequest(
	ctx context.Context,
	method, key string,
	query url.Values,
	body io.ReadSeeker,
	payloadHash string,
	meta map[string]string,
	ifNoneMatch bool,
) (*http.Request, error) {
	reqURL, host, canonicalURI, canonicalQuery := c.requestURL(key, query)
	if payloadHash == "" {
		payloadHash = emptySHA256
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	if c.cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.cfg.SessionToken)
	}
	for key, value := range meta {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		name := "X-Amz-Meta-" + helpers.UpperFirstRune(strings.TrimSpace(key))
		req.Header.Set(name, trimmed)
	}
	if ifNoneMatch {
		req.Header.Set("If-None-Match", "*")
	}
	canonicalHeaders, signedHeaders := canonicalizeHeaders(host, req.Header)
	req.Header.Set("Authorization", c.signRequest(method, canonicalURI, canonicalQuery, amzDate, payloadHash, canonicalHeaders, signedHeaders))
	return req, nil
}

// requestURL builds the request URL and canonical components.
func (c *Client) requestURL(key string, query url.Values) (string, string, string, string) {
	endpoint := c.cfg.Endpoint
	parsed, _ := url.Parse(endpoint)
	host := parsed.Host
	key = strings.TrimLeft(key, "/")

	var objectPath string
	if c.cfg.PathStyle {
		if key == "" {
			objectPath = "/" + c.cfg.Bucket
		} else {
			objectPath = "/" + c.cfg.Bucket + "/" + key
		}
	} else {
		host = c.cfg.Bucket + "." + host
		objectPath = "/" + key
	}

	canonicalURI := encodePath(objectPath)
	canonicalQuery := canonicalizeQuery(query)
	reqURL := endpoint + objectPath
	if !c.cfg.PathStyle && parsed.Scheme != "" {
		reqURL = parsed.Scheme + "://" + host + objectPath
	}
	if canonicalQuery != "" {
		reqURL += "?" + canonicalQuery
	}
	return reqURL, host, canonicalURI, canonicalQuery
}

// signRequest builds the AWS SigV4 Authorization header value.
func (c *Client) signRequest(
	method string,
	canonicalURI string,
	canonicalQuery string,
	amzDate string,
	payloadHash string,
	canonicalHeaders string,
	signedHeaders string,
) string {
	date := amzDate[:8]
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", date, c.cfg.Region)
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := deriveSigningKey(c.cfg.SecretKey, date, c.cfg.Region)
	signature := hmacSHA256Hex(signingKey, stringToSign)
	return fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey,
		scope,
		signedHeaders,
		signature,
	)
}

// canonicalizeHeaders returns canonical and signed header strings.
func canonicalizeHeaders(host string, headers http.Header) (string, string) {
	entries := map[string]string{
		"host": normalizeHeaderValue([]string{host}),
	}
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		entries[lower] = normalizeHeaderValue(values)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteString(":")
		canonical.WriteString(entries[name])
		canonical.WriteString("\n")
	}

	return canonical.String(), strings.Join(names, ";")
}

// normalizeHeaderValue trims and collapses whitespace in header values.
func normalizeHeaderValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

// canonicalizeQuery returns the canonical query string.
func canonicalizeQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(values))
	for _, key := range keys {
		vals := values[key]
		sort.Strings(vals)
		for _, value := range vals {
			pairs = append(pairs, awsEncode(key)+"="+awsEncode(value))
		}
	}
	return strings.Join(pairs, "&")
}

// awsEncode encodes a query value according to AWS canonical rules.
func awsEncode(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	escaped = strings.ReplaceAll(escaped, "%7E", "~")
	return escaped
}

// encodePath encodes each path segment for signature calculations.
func encodePath(value string) string {
	if value == "" {
		return "/"
	}
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		segments[i] = pathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// pathEscape escapes a path segment while preserving slashes.
func pathEscape(value string) string {
	if value == "" {
		return ""
	}
	return strings.ReplaceAll(url.PathEscape(value), "%2F", "/")
}

// deriveSigningKey derives the signing key for the given date and region.
func deriveSigningKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}

// hmacSHA256 returns the HMAC-SHA256 of data using key.
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

// hmacSHA256Hex returns the hex-encoded HMAC-SHA256 of data.
func hmacSHA256Hex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

// hashReader returns the SHA256 hash of the reader's contents.
func hashReader(r io.Reader) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
