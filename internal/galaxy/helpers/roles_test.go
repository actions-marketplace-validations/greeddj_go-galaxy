package helpers

import (
	"strings"
	"testing"
)

func TestIsRoleNamePart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"geerlingguy", true}, {"php-versions", true}, {"nginx_core", true}, {"6connect", true}, {"Foo", true},
		{"a", true}, {strings.Repeat("a", roleNamePartMaxLen), true},
		{"", false}, {"-lead", false}, {"_lead", false}, {"a.b", false}, {"a b", false}, {"a/b", false},
		{"a\n", false}, {"ü", false}, {strings.Repeat("a", roleNamePartMaxLen+1), false},
	} {
		if got := IsRoleNamePart(tt.in); got != tt.want {
			t.Errorf("IsRoleNamePart(%q) = %t, want %t", tt.in, got, tt.want)
		}
	}
}

func TestSplitRoleName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in, owner, role string
		ok              bool
	}{
		{"geerlingguy.docker", "geerlingguy", "docker", true},
		{" geerlingguy.php-versions ", "geerlingguy", "php-versions", true},
		{"6connect.x", "6connect", "x", true},
		{"docker", "", "", false},
		{"a.b.c", "", "", false},
		{".docker", "", "", false},
		{"geerlingguy.", "", "", false},
		{"geerlingguy.-x", "", "", false},
		{"geerling guy.docker", "", "", false},
	} {
		owner, role, ok := SplitRoleName(tt.in)
		if owner != tt.owner || role != tt.role || ok != tt.ok {
			t.Errorf("SplitRoleName(%q) = (%q, %q, %t), want (%q, %q, %t)", tt.in, owner, role, ok, tt.owner, tt.role, tt.ok)
		}
		if IsRoleName(tt.in) != tt.ok {
			t.Errorf("IsRoleName(%q) = %t, want %t", tt.in, !tt.ok, tt.ok)
		}
	}
}

func TestIsRoleInstallName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"geerlingguy.docker", true}, {"ansible-role-docker", true}, {"_private", true}, {"Role.Name-1", true},
		{"a", true}, {strings.Repeat("a", roleInstallNameMaxLen), true},
		{"", false}, {".", false}, {"..", false}, {".hidden", false}, {"-opt", false}, {"a/b", false},
		{"a b", false}, {"a\tb", false}, {"ansible_collections", false}, {"a:b", false},
		{strings.Repeat("a", roleInstallNameMaxLen+1), false},
	} {
		if got := IsRoleInstallName(tt.in); got != tt.want {
			t.Errorf("IsRoleInstallName(%q) = %t, want %t", tt.in, got, tt.want)
		}
		if got := IsRoleInstallName(tt.in); got && !IsPathElement(tt.in) {
			t.Errorf("IsRoleInstallName(%q) accepts what IsPathElement refuses", tt.in)
		}
	}
	// Every Galaxy role name is an install name: the default for a Galaxy
	// role is the name itself.
	for _, name := range []string{"geerlingguy.docker", "6connect.x", "Acme.php-versions", "ns.nginx_core"} {
		if !IsRoleName(name) || !IsRoleInstallName(name) {
			t.Errorf("%q: IsRoleName=%t IsRoleInstallName=%t, want both true", name, IsRoleName(name), IsRoleInstallName(name))
		}
	}
}

func TestIsRoleVersion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"1.2.3", true}, {"v1.2.3", true}, {"master", true}, {"release/1.x", true}, {"HEAD", true},
		{"1.2.3+build.4", true}, {"0123456789abcdef0123456789abcdef01234567", true}, {"a_b", true},
		{"", false}, {".hidden", false}, {"-x", false}, {"/abs", false}, {"a//b", false}, {"a..b", false},
		{"a/", false}, {"v1.lock", false}, {"a b", false}, {"a\n", false}, {"a:b", false}, {"a~1", false},
		{strings.Repeat("1", roleVersionMaxLen+1), false},
	} {
		if got := IsRoleVersion(tt.in); got != tt.want {
			t.Errorf("IsRoleVersion(%q) = %t, want %t", tt.in, got, tt.want)
		}
	}
}

// TestRoleArtifactFilenameNeverCollidesWithACollection pins the invariant
// RoleArtifactFilename's doc comment states: the text before the first "-"
// carries a dot, which no collection namespace can.
func TestRoleArtifactFilenameNeverCollidesWithACollection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, version string }{
		{"geerlingguy.docker", "7.4.1"}, {"ansible-role-docker", "main"}, {"x", "release/1.x"},
	} {
		got := RoleArtifactFilename(tt.name, tt.version)
		head, _, ok := strings.Cut(got, "-")
		if !ok || !strings.Contains(head, ".") {
			t.Errorf("RoleArtifactFilename(%q, %q) = %q: the leading segment must carry a dot", tt.name, tt.version, got)
		}
		if IsCollectionNamePart(head) {
			t.Errorf("RoleArtifactFilename(%q, %q) = %q: leading segment reads as a collection namespace", tt.name, tt.version, got)
		}
		if !strings.HasSuffix(got, ".tar.gz") || !strings.HasPrefix(got, "role.") {
			t.Errorf("RoleArtifactFilename(%q, %q) = %q", tt.name, tt.version, got)
		}
	}
	if got, want := RoleArtifactFilename("geerlingguy.docker", "7.4.1"), "role.geerlingguy.docker-7.4.1.tar.gz"; got != want {
		t.Errorf("RoleArtifactFilename = %q, want %q", got, want)
	}
}
