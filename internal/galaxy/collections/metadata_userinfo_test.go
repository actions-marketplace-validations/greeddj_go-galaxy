package collections

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// metadataURLPassword is the credential these tests smuggle into a
// server-supplied metadata reference. Distinctive on purpose, like
// galaxy_info_test.go's urlPassword: a substring search for it can then only
// succeed by finding the value itself.
const metadataURLPassword = "sup3rsecret"

// normalizeVersionsURLCase is one table entry for
// TestNormalizeVersionsURLRefusesUserinfo: a (source, versionsURL) pair and
// the sentinel it must carry, or nil for a pair that must be accepted.
type normalizeVersionsURLCase struct {
	wantErr     error
	name        string
	source      string
	versionsURL string
}

// normalizeVersionsURLCases pairs each userinfo-bearing shape with its own
// userinfo-free twin, in this same table rather than in a separate test: the
// twin differs only by the deleted credential, so it is the positive control
// that keeps each refusal from being indistinguishable from a pair the guard
// never judged at all.
//
// Three shapes, one per way a credential can reach the output: the
// versions_url a root metadata document declares, the href its
// highest_version names (an absolute value resolved the same way, which is
// what makes it a distinct entry point rather than a duplicate row), and a
// relative reference whose credential comes from the source it is resolved
// against - the shape that only a check on this function's OUTPUT can catch,
// since neither input carries userinfo where the caller could see it.
func normalizeVersionsURLCases() []normalizeVersionsURLCase {
	const userinfo = "u:" + metadataURLPassword + "@"
	return []normalizeVersionsURLCase{
		{
			name:        "absolute versions_url with userinfo refused",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://" + userinfo + "hub.example/api/v3/collections/acme/widgets/versions/",
			wantErr:     helpers.ErrMetadataURLUserinfo,
		},
		{
			name:        "absolute versions_url without userinfo accepted",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://hub.example/api/v3/collections/acme/widgets/versions/",
			wantErr:     nil,
		},
		{
			name:        "highest_version href with userinfo refused",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://" + userinfo + "hub.example/api/v3/collections/acme/widgets/versions/1.0.0/",
			wantErr:     helpers.ErrMetadataURLUserinfo,
		},
		{
			name:        "highest_version href without userinfo accepted",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://hub.example/api/v3/collections/acme/widgets/versions/1.0.0/",
			wantErr:     nil,
		},
		{
			name:        "relative reference against a source with userinfo refused",
			source:      "https://" + userinfo + "hub.example/api/v3",
			versionsURL: "collections/acme/widgets/versions/",
			wantErr:     helpers.ErrMetadataURLUserinfo,
		},
		{
			name:        "relative reference against a source without userinfo accepted",
			source:      "https://hub.example/api/v3",
			versionsURL: "collections/acme/widgets/versions/",
			wantErr:     nil,
		},
	}
}

// TestNormalizeVersionsURLRefusesUserinfo drives the guard through the
// function that owns it, over every shape in normalizeVersionsURLCases.
//
// Killing mutation, run: deleting the checkMetadataURLUserinfo call from
// normalizeVersionsURL (returning base, nil) fails all three refusal rows,
// leaving the three accepting rows green:
//
//	metadata_userinfo_test.go:113: normalizeVersionsURL("https://hub.example/api/v3",
//	"https://u:sup3rsecret@hub.example/api/v3/collections/acme/widgets/versions/")
//	err = <nil>, want galaxy metadata url must not contain userinfo
func TestNormalizeVersionsURLRefusesUserinfo(t *testing.T) {
	t.Parallel()

	for _, tt := range normalizeVersionsURLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeVersionsURL(tt.source, tt.versionsURL)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("normalizeVersionsURL(%q, %q) err = %v, want nil (got %q)", tt.source, tt.versionsURL, err, got)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("normalizeVersionsURL(%q, %q) err = %v, want %v", tt.source, tt.versionsURL, err, tt.wantErr)
			}
		})
	}
}

// TestNormalizeVersionsURLDoesNotEchoCredential is
// TestCheckDownloadURLDoesNotEchoCredential's twin on the metadata boundary,
// and it has the same shape for the same reason: three independent negative
// checks and one positive check, none of them a t.Fatalf chain, so a message
// that leaks the credential and a message that names nothing at all are
// reported as the separate defects they are.
//
// Killing mutation, run: rendering raw instead of display in
// checkMetadataURLUserinfo fails all four checks - the three negative ones on
// the value it now renders, and the positive one because the raw value does
// not contain the cut form that check looks for:
//
//	metadata_userinfo_test.go:148: refusal message contains the password:
//	galaxy metadata url must not contain userinfo:
//	"https://u:sup3rsecret@hub.example/api/v3/collections/acme/widgets/versions/?X-Amz-Signature=deadbeef#frag"
//
// The other three lines label that same rendered value differently.
func TestNormalizeVersionsURLDoesNotEchoCredential(t *testing.T) {
	t.Parallel()

	raw := "https://u:" + metadataURLPassword + "@hub.example/api/v3/collections/acme/widgets/versions/" +
		"?X-Amz-Signature=deadbeef#frag"
	_, err := normalizeVersionsURL("https://hub.example/api/v3", raw)
	if err == nil {
		t.Fatal("normalizeVersionsURL accepted a userinfo-bearing metadata URL, want a refusal")
	}
	msg := err.Error()

	if strings.Contains(msg, metadataURLPassword) {
		t.Errorf("refusal message contains the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("refusal message contains the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("refusal message contains the presigned query: %s", msg)
	}
	if !strings.Contains(msg, "https://hub.example/api/v3/collections/acme/widgets/versions/") {
		t.Errorf("refusal message does not name the refused URL's origin and path: %s", msg)
	}
}

// TestNormalizeVersionsURLPassesThroughUnparseable pins the deliberate
// pass-through on checkMetadataURLUserinfo's parse arm: a value url.Parse
// refuses is returned with no error, even carrying what looks like a
// credential, rather than being refused the way checkDownloadURL refuses its
// own unparseable input.
//
// The url.Parse assertion is a positive control on the fixture rather than a
// restatement of the code: without it, "no error was returned" would be
// indistinguishable from a value that simply parsed cleanly and carried no
// userinfo, which is not the arm this test exists to pin.
//
// What the pass-through costs is a classified refusal, not containment -
// net/http parses the same value and refuses to build a request from it, so
// nothing is fetched and no credential is composed - and it is the same
// residual normalizeVersionsURL discloses for the one shape it deliberately
// leaves outside its rule, a metadata URL naming a scheme this tool does not
// speak. It costs no rendering: the *url.Error that refused build produces is
// dropped for helpers.ErrMetadataRequestBuildFailed, which names no part of
// the value, and every line this program composes about such a value cuts it
// instead - which is exactly why those cuts are textual and wait on no parse.
func TestNormalizeVersionsURLPassesThroughUnparseable(t *testing.T) {
	t.Parallel()

	// A raw control character in the authority is what url.Parse refuses. It is
	// spliced in at run time rather than written into a constant expression,
	// because SA1007 flags a url.Parse over a constant it can evaluate as
	// invalid - which is the right call for production code and would here
	// only be reporting the fixture this test is built on.
	// #nosec G101 -- test fixture literal, not a real credential
	raw := "https://u:" + metadataURLPassword + "@hub.exa" + string([]byte{0x7f}) + "mple/versions/"
	if _, parseErr := url.Parse(raw); parseErr == nil {
		t.Fatalf("control: url.Parse(%q) succeeded, so this fixture does not reach the pass-through arm", raw)
	}

	got, err := normalizeVersionsURL("https://hub.example/api/v3", raw)
	if err != nil {
		t.Fatalf("normalizeVersionsURL(unparseable) err = %v, want nil", err)
	}
	if got != raw {
		t.Fatalf("normalizeVersionsURL(unparseable) = %q, want it returned unchanged: %q", got, raw)
	}
}

// unreachableCandidateMatch is a successMatch newFallThroughServer can never
// route to, so the server built with it answers 404 for every root-metadata
// candidate: no apiRoot candidate path contains it.
const unreachableCandidateMatch = "/no-such-candidate/"

// rootBodyWithVersionsURL renders the minimal root metadata document
// loadCollectionMetadata needs, pointing both the versions_url and the
// highest_version href at versionsURL.
func rootBodyWithVersionsURL(versionsURL string) string {
	return `{"versions_url":"` + versionsURL +
		`","highest_version":{"href":"` + versionsURL + `1.0.0/","version":"1.0.0"}}`
}

// TestLoadRootMetadataWalkUnaffectedByMetadataURLGuard proves the guard's
// placement: it sits downstream of the status-routed server walk, so a 404 on
// the first server still advances to the second, and the refusal that follows
// is the metadata-URL verdict rather than the 404 the first server produced.
//
// Both servers come from metadata_test.go's own newFallThroughServer: the
// first with a successMatch no candidate path can contain, so it 404s
// everything, the second serving a root metadata document whose versions_url
// embeds a credential.
//
// The control run keeps that walk - a 404-only first server, a second server
// serving root metadata - and replaces the poisoned versions_url with one that
// is both credential-free and reachable, naming a third httptest server
// started for the purpose. The third server is not decoration: "objects.example"
// resolves nowhere, so a control that merely deleted the credential would fail
// on DNS and prove nothing. The two runs therefore differ in more than one
// value, and the control's claim is the narrower one that follows from that:
// this wiring can carry a run all the way through the walk to a served version
// metadata document, which is what makes the refusal above the guard's doing
// rather than the fixture's.
func TestLoadRootMetadataWalkUnaffectedByMetadataURLGuard(t *testing.T) {
	t.Parallel()
	srvA, seenA := newFallThroughServer(t, unreachableCandidateMatch, "")
	poisoned := "https://u:" + metadataURLPassword + "@objects.example/api/v2/collections/acme/widgets/versions/"
	srvB, seenB := newFallThroughServer(t, "/api/v2/", rootBodyWithVersionsURL(poisoned))

	cfg := &config.Config{
		Server:  srvA.URL,
		Servers: []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL}},
	}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	// Either server's client would do - both are plain HTTP httptest servers -
	// so one is picked rather than a client per server being threaded through.
	runtime := infra.New(noopPrinter{}, srvB.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	_, err := loadCollectionMetadata(context.Background(), deps, col)
	if !errors.Is(err, helpers.ErrMetadataURLUserinfo) {
		t.Fatalf("loadCollectionMetadata err = %v, want helpers.ErrMetadataURLUserinfo", err)
	}
	var statusErr *cacheManager.HTTPStatusError
	if errors.As(err, &statusErr) {
		t.Errorf("loadCollectionMetadata reported the first server's HTTP status (%d) instead of the guard's verdict: %v",
			statusErr.Code, err)
	}
	if len(seenA()) == 0 {
		t.Errorf("the first server received no request, so this run never exercised the walk at all")
	}
	if len(seenB()) == 0 {
		t.Errorf("the second server received no request, so the walk did not advance past the first server's 404")
	}

	assertCleanVersionsURLWalkSucceeds(t, srvA)
}

// assertCleanVersionsURLWalkSucceeds is the control described on
// TestLoadRootMetadataWalkUnaffectedByMetadataURLGuard. It reuses the
// already-running 404-only server as the first candidate and builds the rest
// fresh: a second server serving root metadata, and a third for that
// document's versions_url to point at - credential-free and, unlike the
// poisoned fixture's "objects.example", actually reachable. That third server
// is what makes this a reachable-walk control rather than a twin differing in
// one value.
func assertCleanVersionsURLWalkSucceeds(t *testing.T, srvA *httptest.Server) {
	t.Helper()
	// "{}" rather than a fuller document: the control only needs the version
	// metadata GET at the end of the walk to succeed, and every field this run
	// reads off it is optional.
	srvVersions, _ := newFallThroughServer(t, "/api/v2/", "{}")
	clean := srvVersions.URL + "/api/v2/collections/acme/widgets/versions/"
	srvB, _ := newFallThroughServer(t, "/api/v2/", rootBodyWithVersionsURL(clean))

	cfg := &config.Config{
		Server:  srvA.URL,
		Servers: []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL}},
	}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srvB.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, err := loadCollectionMetadata(context.Background(), deps, col); err != nil {
		t.Fatalf("control: loadCollectionMetadata with a credential-free versions_url = %v, want nil", err)
	}
}
