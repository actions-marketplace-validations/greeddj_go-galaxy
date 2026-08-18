package helpers

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// transportFixturePassword is the credential the fixture below smuggles into
// the request URL. Distinctive on purpose: a substring search for a value that
// can collide with nothing else in a rendered message is an answer rather than
// a coincidence.
const transportFixturePassword = "pa55w0rd-must-not-be-rendered"

// transportFixtureHostPath is the part of that fixture an operator reading the
// failure actually needs, and the part no cut here removes.
const transportFixtureHostPath = "objects.example/acme-widgets-1.0.0.tar.gz"

// transportFixtureURL carries both credential-bearing parts of a URL at once,
// so one fixture covers both halves of the cut under test.
const transportFixtureURL = "https://u:" + transportFixturePassword + "@" + transportFixtureHostPath +
	"?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900"

// errTransportFixtureCause is the transport failure the fixture *url.Error
// carries, standing in for a refused dial. It is a sentinel rather than a
// rendered string so the classification check below asks whether the cause is
// still REACHABLE, not merely whether it is still printed.
var errTransportFixtureCause = errors.New("connect: connection refused")

// TestCutTransportURLRendersTheURLWithItsCredentialsCut pins what
// CutTransportURL puts in front of an operator: the URL the caller asked for,
// with both credential-bearing parts gone, and the transport's own cause
// beside it.
//
// The fixture *url.Error is built by hand rather than obtained from an
// http.Client, and the URL field is deliberately spelled the way net/http
// would spell it - password masked, query intact - so this test asserts
// against what that redaction actually leaves rather than against a
// convenient value. That is the whole premise of the cut: net/http's own
// masking covers the password and nothing else.
//
// The five checks are independent t.Errorf calls rather than a t.Fatalf chain:
// a message that carries a capability, one that names nothing at all, and one
// whose cause stopped being reachable are three different defects with three
// different remedies.
//
// The host-and-path check is the positive control. It stays green under the
// mutation below, since the uncut value contains that substring too, which is
// what makes it a control on the cut rather than a second pin of it.
//
// KILLING MUTATION, run: assigning rawURL to display in place of
// WithoutCredentials(rawURL) fails the three negative checks. The first reads:
//
//	transporturl_test.go:66: rendered message carries the password: Get
//	"https://u:pa55w0rd-must-not-be-rendered@objects.example/acme-widgets-1.0.0.tar.gz?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900":
//	connect: connection refused
func TestCutTransportURLRendersTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	masked := strings.Replace(transportFixtureURL, transportFixturePassword, "***", 1)
	err := CutTransportURL(transportFixtureURL, &url.Error{Op: "Get", URL: masked, Err: errTransportFixtureCause})
	msg := err.Error()

	if strings.Contains(msg, transportFixturePassword) {
		t.Errorf("rendered message carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("rendered message carries the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("rendered message carries the presigned query: %s", msg)
	}
	if !strings.Contains(msg, transportFixtureHostPath) {
		t.Errorf("rendered message does not name the host and path it failed to reach: %s", msg)
	}
	if !errors.Is(err, errTransportFixtureCause) {
		t.Errorf("the transport cause is no longer reachable through the wrapper: %v", err)
	}
}

// TestCutTransportURLLeavesANonURLErrorAlone pins the arm that does nothing.
// CutTransportURL is applied at a call site that cannot know what
// http.Client.Do returned, so an error that is not a *url.Error has to come
// back as it was - identical value, not merely an equal message - since
// re-rendering one would name a URL that error never claimed to be about.
func TestCutTransportURLLeavesANonURLErrorAlone(t *testing.T) {
	t.Parallel()

	if got := CutTransportURL(transportFixtureURL, errTransportFixtureCause); !errors.Is(got, errTransportFixtureCause) {
		t.Fatalf("CutTransportURL(plain error) = %v, want it returned unchanged", got)
	}
	// Identity, not just errors.Is: a wrapper that rendered a display around
	// this error would still satisfy the check above.
	if got := CutTransportURL(transportFixtureURL, errTransportFixtureCause); got.Error() != errTransportFixtureCause.Error() {
		t.Fatalf("CutTransportURL(plain error).Error() = %q, want %q", got.Error(), errTransportFixtureCause.Error())
	}
}
