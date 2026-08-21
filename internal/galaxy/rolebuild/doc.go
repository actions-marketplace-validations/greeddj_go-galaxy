// Package rolebuild turns the source tree of an Ansible role into the tar.gz
// artifact the rest of the pipeline caches and extracts. It owns the role
// half of what ansible-galaxy does when it archives a role fetched from git:
// the tree at the repository root becomes a deterministic tar.gz through
// internal/galaxy/treearchive, with no MANIFEST.json or FILES.json leading it
// (a role has neither) and every entry at the archive top level, so the
// extractor lays the role out under its install directory with no prefix to
// strip and no directory to guess, where ansible searches the archive for the
// shortest parent of a meta/main.yml.
//
// What identifies the tree as a role is read the way ansible's GalaxyRole
// reads it: meta/main.yml, else meta/main.yaml, each counted only as a regular
// file; a tree carrying both is refused under helpers.ErrRoleMetaInvalid
// rather than silently read through the first, and a tree carrying neither,
// or no meta directory, is helpers.ErrRoleMetaNotFound. Its dependencies:
// list is read in every shape ansible's role_yaml_parse accepts - a plain
// string, an old-style mapping under role:, a mapping under name:, src:, scm:
// and version: - and normalized into gitsource.RoleDependency values, which
// carry each field as written: a string spec is not split at its commas and a
// scm+url src is not split at its plus, because the requirements grammar the
// caller already validates a roles: entry with is the one place that spelling
// is judged. The keys ansible drops from a spec (when:, role variables) are
// dropped here too, without a warning, since ansible passes them on as role
// parameters rather than complaining. meta/requirements.yml (or .yaml) is
// read the same way when present and its list is appended behind the
// dependencies, as ansible-galaxy appends role.requirements behind
// role.metadata_dependencies. galaxy_info.role_name is carried as
// information; the install name is the caller's, as it is in ansible.
//
// The exclusion list is two rules: a directory named .git at any depth, and
// meta/.galaxy_install_info at the root. The git tree reader already refuses
// a .git entry by name, so that rule is belt and braces for a Source that is
// not a git tree; the install record is ansible-galaxy's own output, which
// a repository commits by accident often enough, and the install writes a
// fresh one after materialization rather than carrying a stale one in the
// artifact. The deliberate divergences
// from ansible are each a narrower reading, not a substitute: only the
// repository root is a role, a nested meta/main.yml is never searched for
// (ansible's shortest-parent scan exists for archives built by hand);
// .gitattributes export-ignore is not honored, where ansible's `git archive`
// path would honor it, because nothing here reads attributes; meta/main.json
// is not read; and a scalar version: is taken as the text written, so 2.0
// stays "2.0" where ansible's str() would render a float.
//
// The tree is read through treearchive.Source, never from the filesystem, so
// nothing here opens a path, follows a real symlink or execs anything. Every
// artifact is probed back through archive.ProbeTarGz before it is handed
// over; a failure there is a defect of this package, reported under
// helpers.ErrGitArtifactSelfCheck with the cause as text so it never
// classifies as an integrity failure of the remote.
package rolebuild
