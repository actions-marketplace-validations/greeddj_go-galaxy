package s3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// slowDownErrorBody is a representative S3 XML <Error> document: a 503
// response body carrying a throttling error code and a human-readable
// message, exactly the shape s3StatusError is meant to surface.
const slowDownErrorBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message></Error>`

// TestGetObjectSurfacesXMLErrorDetails proves that a non-2xx GET response
// carrying an S3 XML error body has its Code and Message folded into the
// returned error, while the error stays matchable against errS3GetFailed via
// errors.Is.
func TestGetObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-error-object"
	fake.failNextWithBody(key, http.MethodGet, http.StatusServiceUnavailable, 1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestDeleteObjectSurfacesXMLErrorDetails proves the same enrichment applies
// to deleteObject, exercising a second call site beyond getObject.
func TestDeleteObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "delete-error-object"
	fake.failNextWithBody(key, http.MethodDelete, http.StatusServiceUnavailable, 1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := b.client.deleteObject(ctx, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3DeleteFailed) {
		t.Fatalf("expected errors.Is(err, errS3DeleteFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestPutObjectSurfacesXMLErrorDetails proves handlePutResponse's default
// (non-2xx, non-precondition, non-not-found) branch enriches its error the
// same way, covering an overwrite PUT that the backend rejects with a
// throttling response.
func TestPutObjectSurfacesXMLErrorDetails(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "put-error-object"
	fake.failNextWithBody(key, http.MethodPut, http.StatusServiceUnavailable, 1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	payload := []byte("payload")
	err := b.client.putObject(ctx, key, bytes.NewReader(payload), int64(len(payload)), "", "", nil, false, "")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3PutFailed) {
		t.Fatalf("expected errors.Is(err, errS3PutFailed), got %v", err)
	}
	assertContainsCodeAndMessage(t, err, "SlowDown", "Please reduce your request rate.")
}

// TestGetObjectFallsBackToStatusOnlyOnMalformedBody proves that a non-2xx
// body that is not a valid S3 XML error document yields the original
// status-only error form, without ever propagating the underlying
// XML-parse failure, and without disturbing errors.Is matching.
func TestGetObjectFallsBackToStatusOnlyOnMalformedBody(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-malformed-body"
	fake.failNextWithBody(key, http.MethodGet, http.StatusInternalServerError, 1, []byte("not xml at all"))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for a malformed body, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), http.StatusText(http.StatusInternalServerError)) {
		t.Fatalf("expected the status text in the fallback error, got %q", err.Error())
	}
}

// TestGetObjectFallsBackToStatusOnlyOnEmptyBody proves the same fallback for
// a response with no body at all (the common case for many real S3 error
// responses returned without a body, e.g. from a misconfigured proxy).
func TestGetObjectFallsBackToStatusOnlyOnEmptyBody(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "get-empty-body"
	fake.failNext(key, http.MethodGet, http.StatusBadGateway, 1)

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := getObjectError(ctx, t, b, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3GetFailed) {
		t.Fatalf("expected errors.Is(err, errS3GetFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for an empty body, got %q", err.Error())
	}
}

// TestHeadObjectStaysStatusOnly proves headObject never appends a code or
// message even when a body is armed on the forced failure: HEAD responses
// carry no entity body (net/http elides it), so headObject's error is the
// status-only form by construction, not merely by the fake choosing not to
// send one.
func TestHeadObjectStaysStatusOnly(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	const key = "head-error-object"
	fake.failNextWithBody(key, http.MethodHead, http.StatusServiceUnavailable, 1, []byte(slowDownErrorBody))

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err := b.client.headObject(ctx, key)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, errS3HeadFailed) {
		t.Fatalf("expected errors.Is(err, errS3HeadFailed), got %v", err)
	}
	if strings.Contains(err.Error(), "(") {
		t.Fatalf("expected a status-only error for a HEAD response, got %q", err.Error())
	}
}

// assertContainsCodeAndMessage fails the test unless err's message contains
// both code and message, which s3StatusError appends in parentheses after
// the HTTP status line.
func assertContainsCodeAndMessage(t *testing.T, err error, code, message string) {
	t.Helper()
	got := err.Error()
	if !strings.Contains(got, code) {
		t.Fatalf("expected error to contain code %q, got %q", code, got)
	}
	if !strings.Contains(got, message) {
		t.Fatalf("expected error to contain message %q, got %q", message, got)
	}
}

// getObjectError calls getObject and returns the resulting error. getObject
// already closes the response body and returns a nil *http.Response on every
// error path, so there is nothing to close here in practice; the nil check
// simply keeps this helper correct even if that contract ever changes.
func getObjectError(ctx context.Context, t *testing.T, b *Backend, key string) error {
	t.Helper()
	resp, err := b.client.getObject(ctx, key)
	if resp != nil {
		defer func() {
			_ = resp.Body.Close()
		}()
	}
	return err
}
