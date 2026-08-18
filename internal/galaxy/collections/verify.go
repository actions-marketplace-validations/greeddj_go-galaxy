package collections

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/psvmcc/hub/pkg/types"
)

// serverSignatureField is the one key of a Galaxy version-metadata signature
// entry this tool reads: the detached signature itself. A server sends more
// alongside it - a fingerprint, a signing service, a timestamp - none of which
// decides anything here, since the keyring decides which key counts.
const serverSignatureField = "signature"

// verifyContext is one run's signature verification state: the policy, the key
// material it verifies against, the client that fetches a requirement's own
// sources, and those sources per collection.
//
// A non-nil *verifyContext always means verification is on. newVerifyContext
// returns nil for a run that verifies nothing, so no caller has to re-ask
// whether the policy was enabled, and enabled() is nil-receiver-safe for
// exactly that reason.
//
// It is built once per run and read from every worker. sources is written once,
// at construction, and only read afterward - the identical
// construct-once-read-only argument tlsDispatchTransport.insecureOrigins
// (internal/galaxy/fetch/client.go) makes for its own map, so concurrent
// readers need no lock because there is never a concurrent writer. keyring,
// fetcher and policy are each documented safe for concurrent reads by the
// package that produces them.
type verifyContext struct {
	keyring *signature.Keyring
	fetcher *signature.Fetcher
	// sources maps "<namespace>.<name>" to the signature source URIs that
	// collection's requirements entry named, deduped, in the order the file
	// wrote them. A collection naming none is absent rather than present and
	// empty, so a run whose requirements declare no signatures at all carries an
	// empty map.
	sources map[string][]string
	policy  signature.Policy
	// skippedUnverified counts collections this run skipped as already
	// installed, which are therefore the collections it verified nothing about.
	// It is the one field written after construction, and an atomic because the
	// writers are the install workers themselves; installWithState reads it once,
	// after they have all joined, and reports the total.
	skippedUnverified atomic.Int64
}

// recordSkippedUnverified counts one collection skipped as already installed. It
// is nil-receiver-safe, so the install worker's call site needs no branch for a
// run that verifies nothing - such a run has nothing to report either way.
func (vc *verifyContext) recordSkippedUnverified() {
	if vc == nil {
		return
	}
	vc.skippedUnverified.Add(1)
}

// reportSkippedUnverified says, once per run and on the result tier, how many
// already-installed collections this run left unverified.
//
// It fires only when verification is on and the count is non-zero, which is
// what keeps it out of every ordinary run's output. The line exists for one
// specific run: the first one an operator turns --keyring on over an existing
// workspace, where every collection is already installed, nothing is verified,
// and the per-collection skip lines are on the transient tier that --quiet and
// a non-TTY CI both drop. PersistentPrintf rather than Warnf: this is a fact
// about what the run did, not a defect, and the remedy (a fresh tree, or
// --clear-cache) is the operator's to choose.
func (vc *verifyContext) reportSkippedUnverified(runtime *infra.Infra) {
	if !vc.enabled() {
		return
	}
	skipped := vc.skippedUnverified.Load()
	if skipped == 0 {
		return
	}
	runtime.Output.PersistentPrintf(
		"🔏 %d already-installed collection(s) were skipped and therefore not verified on this run", skipped)
}

// newVerifyContext resolves this run's verification state from cfg, or reports
// why the run cannot proceed. It returns (nil, nil) for a run that verifies
// nothing, which is the default and must stay free: no keyring is read, no HTTP
// client is built, and nothing is gathered.
//
// It is the single point both verifying commands pass through, which is what
// makes three things properties of this function rather than conventions each
// command remembers: the ansible.cfg signature-key warning is emitted once per
// run, the keyring is read once rather than per collection, and one
// signature.Fetcher is built for the whole run - the sharing its own doc
// comment requires, since a fetcher owns a connection pool and one per
// collection would defeat reuse for the many small requests this phase makes.
//
// Whether this run verifies at all is answered by signature.VerificationEnabled
// over the two configured values, before any policy is built, which is what
// keeps the zero value of a Signature block meaning "verify nothing" rather
// than "refuse the run over a count spec nothing will read". Asking that
// package rather than testing the two values here is what keeps the predicate
// one predicate: see VerificationEnabled for what a copy of it would cost.
// The parse it skips is not skipped so much as already done:
// config.validateSignatureConfig builds this exact policy for every command at
// config-build time and keeps only its error, so a malformed count spec or an
// unknown status code has already failed the run under the usage exit class
// before anything here runs.
//
// The policy IS built for a run that verifies, even though that same call
// already happened, because two call sites building one policy from one set of
// values is a disagreement nothing would report if the consuming one stopped
// checking - the argument validateSignatureConfig's own doc comment makes. Its
// error arm is handled rather than assumed unreachable for that reason.
func newVerifyContext(cfg *config.Config, runtime *infra.Infra, roots []collection) (*verifyContext, error) {
	// Emitted before the enabled check, so the operator whose keyring sits in
	// an ansible.cfg - the shape this warning exists for - is told about it on
	// the very run that verifies nothing because of it.
	if warning := cfg.AnsibleSignatureKeysWarning(); warning != "" {
		runtime.Output.Warnf("%s", warning)
	}
	if !signature.VerificationEnabled(cfg.Signature.KeyringPath, cfg.Signature.DisableGPGVerify) {
		return nil, verificationOffError(runtime, cfg.Signature, roots)
	}

	policy, err := signature.NewPolicy(
		cfg.Signature.KeyringPath, cfg.Signature.RequiredCount, cfg.Signature.IgnoreStatusCodes, cfg.Signature.DisableGPGVerify)
	if err != nil {
		return nil, err
	}
	keyring, err := signature.LoadKeyring(policy.KeyringPath)
	if err != nil {
		return nil, err
	}
	announceVerification(cfg, runtime, keyring)
	// Warned after the announcement above, and only once verification is known
	// to be on: a run that verifies nothing fetches nothing, so a warning about
	// what --offline would refuse to fetch would be noise on every ordinary
	// unverified run.
	warnOfflineSignatureSources(cfg, runtime, roots)

	return &verifyContext{
		keyring: keyring,
		fetcher: signature.NewFetcher(cfg.Timeout, cfg.Offline),
		sources: requirementSources(roots),
		policy:  policy,
	}, nil
}

// announceVerification tells the operator, once per run, what this run's
// signature verification state is - a keyring and a required count - on the
// tier and in the spelling its severity earns.
//
// One line rather than two: the fact this states - that this run's
// verification state exists, and that a dry run's own preview validates the
// setup and verifies no collection - has one home, and this function is it,
// rather than a real-run line here and a separate dry-run caveat somewhere
// else that could drift from it.
//
// The tier moves with cfg.DryRun, and each spelling earns its own tier rather
// than sharing one. On a real run this is a configuration echo of work that
// WILL happen - reportSkippedUnverified's own rubric for the result tier: a
// fact about this run, not a defect in it - so PersistentPrintf. On a dry run
// it is a caveat qualifying a report nobody asked to see twice: the same kind
// of thing warnIfFrozenOffline (dryrun.go) already is, so it goes where that
// one does too - Warnf, stderr, surviving --quiet, printed alongside
// dryRunBanner rather than split from it the way a PersistentPrintf spelling
// would be by `go-galaxy install --dry-run > preview.log`.
//
// The message's own claim - that no signature source is fetched and no
// artifact's signatures are checked - holds by construction rather than by
// convention: classifyDryRun, the function that builds a dry run's whole report,
// has no verification branch of its own and downloads nothing to verify, so
// there is no path left by which a preview could reach a signature source in the
// first place.
//
// The caveat disclaims the whole report's would-fail COUNT, not the
// would-act half of it, and that scope is deliberate rather than an
// oversight. installCollection's own canSkipInstall gate means a real
// install's "Up to date" verdict never claimed anything about verification in
// the first place: an already-installed collection is skipped before
// verifyCollectionSignatures is ever reached (install.go). warm carries no
// such gate at all - warmOne runs prepareWithRecovery with
// warmVerifyAndEnsure for every collection, cache hits included - so "Already
// warm" is unfaithful about verification in exactly the way "Up to date" is
// not. A caveat scoped to "collections that would be acted on" would
// therefore be false for warm, and one scoped to "every collection this
// report names" would be false for install's own settled rows; scoping it to
// the would-fail COUNT alone is exactly true for both.
func announceVerification(cfg *config.Config, runtime *infra.Infra, keyring *signature.Keyring) {
	if cfg.DryRun {
		runtime.Output.Warnf(
			"signature verification is configured and this preview validates the setup and verifies no collection: "+
				"keyring %s, required count %s; no signature source is fetched and no artifact's signatures are checked, "+
				"so the would-fail count covers no signature verdict",
			keyring.Path(), cfg.Signature.RequiredCount)

		return
	}
	runtime.Output.PersistentPrintf("🔏 Signature verification on: keyring %s, required count %s",
		keyring.Path(), cfg.Signature.RequiredCount)
}

// warnOfflineSignatureSources warns, once per run, when cfg.Offline is set and
// some requirements root declares a signature source that
// signature.SourceRequiresNetwork says this run cannot fetch offline. It
// names the first such root through requirementKey - deliberately not the
// source URI itself, which the next person adding detail to this message
// will reach for and must not - so the operator sees a concrete place to act
// rather than only learning that one exists somewhere.
//
// It fires on a real run exactly as it does on a preview, and that is the
// whole reason it lives here rather than behind a dry-run-only probe: the
// fact it states - a collection whose gather reaches this source fails
// offline - is equally true of both, and newVerifyContext is the one funnel
// both install and warm pass through before either ever schedules a worker.
//
// A warning rather than a per-collection would-fail verdict, because the
// certain set is narrower than the warned one. nextBlob's first pull is
// always sources[0] (gatherOne), and walkBlobs (signature/verify.go)
// evaluates its early stop only after a successful next() plus checkOne, so a
// FIRST declared source needing the network is an unconditional failure for
// any collection whose gather runs - but a file:// source listed first that
// verifies on its own means a later http(s) source in the same list is never
// fetched at all. A verdict over the warned set would be a false failure for
// exactly that shape; a verdict over the certain set (sources[0] alone) would
// encode a list-position rule no operator reading a report can see. A warning
// over the superset costs one line and traps nobody either way.
//
// A warning rather than the refusal runWarm makes for --no-cache
// (helpers.ErrWarmCacheDisabled), because that combination is unworkable by
// construction - warm's entire product is cache state, so --no-cache leaves
// it nothing to produce - while this one is not: an install --offline run
// whose collections are all already installed skips verification entirely
// (canSkipInstall) and succeeds without ever reaching a source this warns
// about.
func warnOfflineSignatureSources(cfg *config.Config, runtime *infra.Infra, roots []collection) {
	if !cfg.Offline {
		return
	}
	for _, root := range roots {
		for _, source := range root.Signatures {
			if !signature.SourceRequiresNetwork(source) {
				continue
			}
			runtime.Output.Warnf(
				"--offline: %s declares a signature source that must be fetched over the network, which offline mode "+
					"refuses; a collection whose gather reaches one fails rather than installing unverified - "+
					"use a file:// source, or drop --offline",
				requirementKey(root))

			return
		}
	}
}

// verificationOffError decides what a run whose policy is not enabled owes the
// operator: a refusal, a warning, or nothing at all. It returns the error
// newVerifyContext then returns alongside a nil context.
//
// A requirements file that declares signatures while NO keyring is configured
// is refused, which is ansible-galaxy's own verdict: a file asking for
// verification is refused rather than silently installed unverified.
//
// Verification switched off outright is not that case and must not borrow its
// sentinel. helpers.ErrKeyringRequired says no keyring was named at all, and
// --disable-gpg-verify is an operator explicitly switching off a configuration
// that may name one - refusing there would both print a false statement about
// the configuration and make the flag unusable in the one situation it exists
// for. It is warned about instead, naming the first collection whose declared
// sources go unchecked, so the divergence is loud rather than silent; the
// config layer's own disabledWithKeyringWarning covers the other half of the
// same switch, a keyring configured and ignored.
//
// A run that declares no signatures anywhere gets neither, since it asked for
// nothing and got nothing.
func verificationOffError(runtime *infra.Infra, sc config.SignatureConfig, roots []collection) error {
	i := slices.IndexFunc(roots, func(root collection) bool { return len(root.Signatures) > 0 })
	if i < 0 {
		return nil
	}
	named := requirementKey(roots[i])
	if sc.DisableGPGVerify {
		runtime.Output.Warnf(
			"signature verification is disabled; the signature sources %s declares - and any other collection's - will not be checked",
			named)
		return nil
	}

	return fmt.Errorf("%w: %s declares signature sources; configure a keyring with --keyring "+
		"(or GO_GALAXY_KEYRING), or drop the signatures: block", helpers.ErrKeyringRequired, named)
}

// requirementSources collects each root's declared signature sources, keyed by
// "<namespace>.<name>", deduped and in file order.
//
// Order is the file's rather than sorted, because it is the order the gather
// walks and therefore the order a required count is reached in: an operator who
// lists their own source first gets it checked first. The dedupe is a linear
// scan over what is a handful of entries in every real requirements file.
//
// A root declaring nothing contributes no key at all, so the map a typical run
// carries is empty rather than one empty slice per collection.
func requirementSources(roots []collection) map[string][]string {
	sources := make(map[string][]string)
	for _, root := range roots {
		if len(root.Signatures) == 0 {
			continue
		}
		key := requirementKey(root)
		for _, source := range root.Signatures {
			if !slices.Contains(sources[key], source) {
				sources[key] = append(sources[key], source)
			}
		}
	}

	return sources
}

// requirementKey is the "<namespace>.<name>" a requirements entry is addressed
// by, which is what a root's signature sources are keyed under: a root names
// the collection, never a particular version of it.
func requirementKey(col collection) string {
	return col.Namespace + "." + col.Name
}

// enabled reports whether this run verifies signatures, and is safe on a nil
// receiver - which is what a run that verifies nothing carries, so every call
// site is one method call rather than a nil check plus a field read.
func (vc *verifyContext) enabled() bool {
	return vc != nil
}

// verifyCollectionSignatures checks col's artifact against this run's signature
// policy: the detached signatures over its MANIFEST.json, and then - only once
// at least one of them verified - the chain from that manifest down to every
// file the archive carries.
//
// The enabled check is the first statement, so a run that verifies nothing pays
// nothing: no manifest is read out of the tarball, no metadata is forced, and
// nothing is allocated.
//
// What a server carries is gathered out of the version-metadata document, so
// verification of server-supplied signatures has a precondition worth stating:
// that document must be in hand. Under --offline it is in hand only when this
// collection's version-metadata URL is already in the API cache - which the
// bake-then-install pipeline normally leaves it in, since warm's own resolve
// fetched it - and it is NOT in hand for a cache that never resolved that URL,
// or one whose entry aged out at helpers.CacheEntryMaxAge. When it is missing,
// the run installs and says so through warnVacuousPass's metadata arm rather
// than failing: a run that could not learn whether a collection is signed has
// not learned that it is unsigned either, and the strict count spelling is how
// an operator makes that fatal.
//
// manifest.ReadFromTarGz is the only permitted producer of the bytes handed to
// signature.Verify and manifest.VerifyChain. manifest.checkPointerFieldLengths
// (reached through parseChainPointer) rests its own amplification argument on
// the manifest being bounded by helpers.ManifestScanMaxBytes while enforcing no
// such bound itself, and VerifyChain takes the bytes as a parameter, so
// ReadFromTarGz is where that bound is actually applied - along with the
// refusal of a zero-byte manifest, which a detached signature would otherwise
// verify perfectly while saying nothing about the artifact.
//
// The chain walk runs under ctx rather than under the signature budget below,
// and that is not an oversight: it performs no network I/O, so a budget sized
// for fetching signature sources has no business bounding it. Nothing
// normalizes VerifyChain's error either - its contextReader surfaces ctx.Err()
// unwrapped and no call here relabels it - so binding it to that budget would
// end a slow local disk with a bare context.DeadlineExceeded attributed to
// nothing in particular, rather than with the sentinel. Its verdict is
// deliberately not memoized beside the extract marker or anywhere else in the
// cache: this project's trust model treats a cache writer as an attacker, so a
// cached "already verified" is exactly the assertion this function exists to
// refuse to take on faith.
//
// What a verifying run costs, on top of the extraction it already pays for: one
// head scan of the tarball for the manifest, which stops at
// helpers.ManifestScanMaxBytes and in practice at the archive's first entry,
// plus - only once a signature has verified - one full decompress-and-hash pass
// over the artifact for the chain. A vacuous pass costs nothing beyond the head
// scan, since no chain is walked, and a run that verifies nothing costs nothing
// at all. A verdict this function refuses on an artifact-side cause is spent
// twice rather than once: prepareWithRecovery evicts and refetches, and the
// second attempt gathers every requirement source again under a fresh budget,
// since nothing about a gathered blob is cached between attempts.
//
// Three residuals are disclosed rather than closed, and the first is the
// largest of the three. The artifact cache and the extracted store are both
// populated BEFORE this verdict is reached - on a fresh download with an
// extracted store configured, streamDownloadAndExtract promotes the
// content-addressable tree during the download itself - so a refused
// collection leaves both populated while nothing is installed. For the
// extracted store that is benign: it is content-addressed, so its entry is
// reachable only by naming the sha of the bytes it holds. The artifact
// cache is NOT - helpers.ArtifactKey is a server fingerprint plus the
// escaped filename, and internal/galaxy/helpers/cachekeys.go records that
// content-addressing it was considered and rejected - so what stays
// behind is attacker-chosen bytes occupying the NAME a later run looks up,
// and that run serves them from cache without contacting the origin. What
// bounds it is that the same run verifies again from the same trust
// boundary and reaches the same refusal, and that --frozen re-hashes
// against the lockfile pin; what it costs is a cache slot no verdict
// evicts.
//
// The second: a file:// source is read even once the budget below has expired,
// because Fetcher.fetchFile observes no context by design; what that costs is
// one local read of a path the requirements file names, which is the operation
// itself. That method's own doc comment holds the wider version of it, a
// blocking read on a hung mount that no budget can end.
//
// The third is older than this check and belongs beside the other two rather
// than nowhere: the artifact file is opened three times on this path - once to
// read the manifest, once to walk the chain, once to extract - so a local
// writer with access to the cache directory can swap the bytes between them.
// It is the same window verifyPinnedSHA already carries, on the same file, and
// the same disclosed residual extractCollection and cleanup's removeInstalled
// state for a live local writer; closing it would mean holding one descriptor
// across all three passes, which is a change to how the extractor and the chain
// reader take their input rather than to this function.
func verifyCollectionSignatures(ctx context.Context, deps installDeps, col collection, payload installPayload) error {
	vc := deps.verify
	if !vc.enabled() {
		return nil
	}
	tarPath := payload.artifact.Path
	manifestBytes, err := manifest.ReadFromTarGz(tarPath)
	if err != nil {
		return err
	}

	// The budget covers the whole phase - every source gathered plus the
	// public-key check interleaved with each one by signature.walkBlobs - which
	// is what helpers.SignatureFetchDeadline means by one collection's signature
	// phase end to end. The interleaved arithmetic is microseconds against a
	// minute, so covering it costs the fetches nothing.
	budget := deps.runtime.SignatureDeadline()
	sigCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	result, err := signature.Verify(manifestBytes, vc.nextBlob(sigCtx, deps.runtime, col, payload.meta), vc.keyring, vc.policy)
	if err != nil {
		return fmt.Errorf("%s: %w", col.key(), signatureDeadlineError(ctx, sigCtx, budget, err))
	}
	if result.VacuousPass {
		warnVacuousPass(deps.runtime, col, vc.policy.Required, payload.metaUnavailable)
	}
	if result.Verified == 0 {
		return nil
	}
	if err := manifest.VerifyChain(ctx, tarPath, manifestBytes); err != nil {
		return fmt.Errorf("%s: %w", col.key(), err)
	}
	if err := checkManifestAttribution(col, manifestBytes); err != nil {
		return fmt.Errorf("%s: %w", col.key(), err)
	}
	deps.runtime.Output.Debugf("verified %s against %d key(s)", col.key(), result.Verified)

	return nil
}

// warnVacuousPass reports a pass no signature backed, and says which of the two
// facts behind it applies.
//
// The wording covers both shapes signature.Result.VacuousPass documents rather
// than only the commoner one: nothing was gathered at all, or something was
// gathered, failed, and carried a status this run tolerates. "Nothing verified
// it" is true of both, where "no signature was checked" would be false of the
// second.
//
// metaUnavailable is what separates "this collection carries no signatures"
// from "this run could not learn whether it does", and the two must not read
// alike: the second is the shape a server that answered 500, or an --offline
// run with no cached version metadata for this collection, produces - and any
// signature the server does carry was never gathered, so the pass says nothing
// about them. It is a warning rather than a failure for the reason stated on
// verifyCollectionSignatures: refusing here would fail a collection for a fact
// the run never learned, while the strict spelling this line names is the
// operator's own way to make that fatal.
func warnVacuousPass(runtime *infra.Infra, col collection, required signature.CountSpec, metaUnavailable bool) {
	if metaUnavailable {
		runtime.Output.Warnf(
			"Nothing verified %s and the policy passed anyway; its version metadata was unavailable, "+
				"so any signatures the server carries were never gathered - write %q to require that at least one verified",
			col.key(), strictSpelling(required))

		return
	}
	runtime.Output.Warnf(
		"Nothing verified %s and the policy passed anyway; write %q to require that at least one verified",
		col.key(), strictSpelling(required))
}

// The keys of MANIFEST.json's collection_info this package reads, and the key
// of the object holding them. They are literals rather than a struct's json
// tags for the reason readIdentityObject exists at all: this reader has to
// judge the document's own key bytes, which a struct decode hides.
const (
	manifestCollectionInfoKey = "collection_info"
	manifestNamespaceKey      = "namespace"
	manifestNameKey           = "name"
	manifestVersionKey        = "version"
)

// checkManifestAttribution binds the signature's verdict to the collection
// being installed: the signed manifest must declare the namespace, name and
// version this run resolved.
//
// Without it a verified signature answers a question nobody asked. Measured
// before this check existed, both accepted with a nil error: acme.app@1.0.0
// installed from an artifact whose signed manifest declared trusted.lib@2.0.0,
// and acme.app@1.0.0 installed from a signed acme.app@0.0.1 - a signed
// downgrade to a known-vulnerable version, recorded as 1.0.0 in the store, the
// lockfile and GALAXY.yml, exiting 0. This project already ruled on
// manifest-declared identity in the opposite direction: cleanup takes a scanned
// collection's namespace and name from the path components it walked and never
// from the manifest, so that no MANIFEST.json content can redirect a deletion.
// Distrusting that identity when deleting while never comparing it when
// installing is the inconsistency this closes.
//
// It runs only where the caller runs it: inside the arm where at least one
// signature verified, after the chain walk succeeded. The binding is worth
// exactly what the signature is worth - on an unsigned manifest an attacker
// chooses both sides of this comparison, so running it on a vacuous pass would
// manufacture assurance rather than establish any.
//
// The comparison is byte-for-byte on all three components, deliberately not
// normalized: col.Version already satisfies helpers.IsExactVersion, and a
// comparison that tolerated a difference is the one an attacker would aim at.
// A real publisher divergence, if one is ever found, is its own decision with
// evidence behind it rather than a loosening made in advance.
//
// The declared identity is bounded per component through
// helpers.TruncateForMessage and rendered as one %q value: these three strings
// come out of an archive-chosen
// document bounded only by helpers.ManifestScanMaxBytes, and this is the one
// message that renders them. Quoting is not decoration - a version declared as
// "9.9.9\nSuccessfully installed acme.app@1.0.0" otherwise puts a real newline
// into the failure line, which internal/safeout deliberately passes through.
//
// How the three values are READ is readIdentityObject's own argument, and it is
// the larger half of this check: a struct decode of this document is
// bypassable.
func checkManifestAttribution(col collection, manifestJSON []byte) error {
	info, err := readIdentityObject(manifestJSON, manifestCollectionInfoKey)
	if err != nil {
		return err
	}
	namespace, err := readIdentityField(info, manifestNamespaceKey)
	if err != nil {
		return err
	}
	name, err := readIdentityField(info, manifestNameKey)
	if err != nil {
		return err
	}
	version, err := readIdentityField(info, manifestVersionKey)
	if err != nil {
		return err
	}
	if namespace == col.Namespace && name == col.Name && version == col.Version {
		return nil
	}

	// Each component is bounded on its own and the three are quoted as one
	// value, so the line reads like the identity it names rather than like three
	// quoted fragments, while a newline or a control character anywhere in it
	// still renders as an escape.
	declared := helpers.TruncateForMessage(namespace) + "." +
		helpers.TruncateForMessage(name) + "@" + helpers.TruncateForMessage(version)

	return fmt.Errorf("%w: %s vouches for %q",
		helpers.ErrSignatureAttributionMismatch, helpers.ManifestFileName, declared)
}

// readIdentityObject returns the value of the one key named want, refusing a
// document that names it more than once in any spelling.
//
// A struct decode cannot be used for this, and that is the whole reason this
// function exists. encoding/json matches a field name by FOLDING rather than by
// equality, and decodes every matching key of a document into that one field in
// document order, so the last spelling wins - while a reader taking the first,
// which is what Python's json.loads and therefore ansible-galaxy and a Galaxy
// importer do, sees another value entirely. Measured against a struct decode,
// four documents were accepted with a nil error while declaring one collection
// to this tool and another to everyone else: a "COLLECTION_INFO" shadowing an
// honest "collection_info", an identity assembled piecewise across three
// spellings, two exact-duplicate keys where only the second matched, and - one
// level down, which is why readIdentityField applies this same rule - a
// "NAMESPACE" shadowing a "namespace".
//
// The rule is byte-for-byte equality with want, and the refusal covers any
// other key that could be taken for it. "Could be taken for it" is Unicode case
// folding rather than ASCII case, because that is what encoding/json's own
// field matching folds by: a rule phrased against case would be a rule about
// the wrong equivalence and would leave the Kelvin-sign and long-s spellings
// through.
//
// It refuses rather than picking a winner, and the argument is not that some
// particular parser takes the first. It is that a document naming its identity
// more than once, in any spelling, HAS no single identity - it has one per
// parser - and parser-dependence is the entire vector this control exists to
// close. Refusing needs no claim about how every importer resolves duplicates;
// picking a winner would need such a claim to hold forever, for parsers this
// project does not control.
func readIdentityObject(raw []byte, want string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, unreadableIdentity(want, "the document is not a JSON object")
	}

	var found json.RawMessage
	matches := 0
	for dec.More() {
		key, ok := identityKey(dec)
		if !ok {
			return nil, unreadableIdentity(want, "its keys do not read as a JSON object's")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, unreadableIdentity(want, "one of its values does not parse")
		}
		if !strings.EqualFold(key, want) {
			continue
		}
		matches++
		if key == want {
			found = value
		}
	}
	if matches != 1 || found == nil {
		return nil, unreadableIdentity(want,
			fmt.Sprintf("%d of its keys can be read as it, and exactly one may be", matches))
	}

	return found, nil
}

// identityKey reads the next object key as a string, reporting false for
// anything else - a shape a well-formed JSON object cannot produce, and
// therefore the one a malformed document ends this walk on.
func identityKey(dec *json.Decoder) (string, bool) {
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	key, ok := tok.(string)

	return key, ok
}

// readIdentityField returns one string field of the identity object under
// exactly the rule readIdentityObject applies to the object itself, for exactly
// the same reason: encoding/json folds an inner field name as readily as an
// outer one, so a fix applied only at the top level would leave the identical
// bypass one level down.
//
// A value that is not a JSON string is refused rather than coerced: a manifest
// declaring a number or an object where a name belongs has not named one.
func readIdentityField(info json.RawMessage, want string) (string, error) {
	raw, err := readIdentityObject(info, want)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", unreadableIdentity(want, "it is not a string")
	}

	return value, nil
}

// unreadableIdentity is the refusal every shape above renders. It names the key
// at fault and what was wrong with it, and never the document's own bytes: this
// runs over an archive-chosen document bounded only by
// helpers.ManifestScanMaxBytes.
func unreadableIdentity(key, why string) error {
	return fmt.Errorf("%w: %s cannot be read for one %q: %s",
		helpers.ErrSignatureAttributionMismatch, helpers.ManifestFileName, key, why)
}

// strictSpelling renders the strict form of a count spec, which is the value an
// operator writes to close the vacuous pass this run just took. Reaching it
// means the spec is not strict already, since a strict policy cannot pass
// vacuously - it fails outright on a zero count.
//
// A count of zero has no strict form, and advising one would advise a spelling
// that can never pass: measured against signature.Policy.verdict, "+0" is
// refused by the strict clause when nothing verifies and by the equality clause
// when something does, so it fails under every outcome. "+1" is the smallest
// spelling that actually requires a signature, which is what the operator who
// wants this pass closed has to write. Zero being its own shape here is the
// same split verdictReason already makes, for the same clause.
func strictSpelling(spec signature.CountSpec) string {
	if spec.All {
		return "+all"
	}
	if spec.Count < 1 {
		return "+1"
	}

	return "+" + strconv.Itoa(spec.Count)
}

// nextBlob builds the pull signature.Verify gathers col's blobs through: each
// source the requirements file named for it, in file order, then each signature
// the server carried in its own version metadata, in the order it sent them.
//
// It fetches one at a time rather than materializing the set, which is what
// helpers.SignatureMaxSize's own doc comment asks of a verifier: 1 MiB a blob
// and helpers.MaxSignaturesPerCollection of them is 64 MiB resident for one
// collection, times cfg.Workers collections at once. Pulling makes that one
// blob at a time.
//
// The accepted regression that buys, stated rather than hidden: two roots
// naming the same URI fetch it twice, since nothing caches a fetched blob
// across collections. That is deliberate - a run-level blob cache is the
// residency this pull exists to avoid - and it costs one extra request per
// duplicate rather than per collection.
//
// The cap counts every source consumed and every server blob taken, not just
// the ones that survive the dedupe. A repository listing the same URI under a
// thousand spellings would otherwise spend a thousand requests while yielding
// one blob, which is the unbounded round-trip cost
// helpers.MaxSignaturesPerCollection exists to bound.
//
// The dedupe is keyed on a digest of the blob's bytes rather than on the bytes
// themselves, so the set of what has already been checked costs 32 bytes an
// entry instead of putting every gathered blob back in memory. What it saves is
// a public-key operation and a cap slot: signature.Verify already counts
// distinct KEYS, so a repeated blob could never have inflated a required count.
//
// gatherLimit is where helpers.MaxSignaturesPerCollection is applied to the
// combined candidate set - the requirements file's own sources plus offered,
// the full pre-truncation count serverSignatureBlobs returns alongside its
// already-capped blob slice - and a gather the cap actually truncated is
// reported; see that function's own doc comment for why a truncation is a
// warning rather than a reserved share of the budget.
func (vc *verifyContext) nextBlob(
	ctx context.Context,
	runtime *infra.Infra,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
) signature.NextBlob {
	sources := vc.sources[requirementKey(col)]
	server, offered := serverSignatureBlobs(meta)
	limit := gatherLimit(runtime, col, len(sources), offered)

	var (
		next int
		seen [][sha256.Size]byte
	)

	return func() (signature.Blob, bool, error) {
		for next < limit {
			blob, err := vc.gatherOne(ctx, sources, server, next)
			next++
			if err != nil {
				return signature.Blob{}, false, err
			}
			sum := sha256.Sum256(blob.Data)
			if slices.Contains(seen, sum) {
				continue
			}
			seen = append(seen, sum)

			return blob, true, nil
		}

		return signature.Blob{}, false, nil
	}
}

// gatherLimit caps one collection's combined candidate set - the requirements
// file's own declared sources plus whatever the server offered alongside the
// artifact - at helpers.MaxSignaturesPerCollection, and warns when that cap
// actually cut the gather short.
//
// The gather's own order already encodes a trust ordering - the operator's
// declared sources come first, a server's supplement comes second, exactly as
// nextBlob's own doc comment promises - so the budget must never be taken
// FROM the authoritative side to guarantee the supplementary one a share.
// Reserving one would do exactly that: it would either silently truncate the
// operator's own declared list, which is the file this run was told to trust
// first, or force checkSignatureSources' own load-time ceiling down below
// helpers.MaxSignaturesPerCollection, turning a requirements file that is
// legal today into one that is not, purely so a Galaxy server - a documented
// trust boundary of this project, not a party this run owes a guaranteed
// share to - is guaranteed one.
//
// Reservation would not remove the silent truncation either; it would only
// move it. A reserved share still drops whatever exceeds it, and reserving
// one adds a second underived number to reason about alongside the cap
// itself, for no truncation actually closed.
//
// The security-shaped consequence of a starved gather - a policy satisfied
// while nothing that mattered was ever verified - is already reported for
// EVERY route to it, not only this one: signature.Result.VacuousPass and
// warnVacuousPass cover a starved gather exactly as they cover an empty one
// or one whose every blob failed with a tolerated status. What this function
// owes the operator on top of that is narrower and specific to here: the fact
// that the cap itself, rather than the policy, is what stopped the gather
// before every candidate already in hand was taken.
//
// sources is the caller's own count of a requirements entry's declared
// sources, never truncated by anything in this package. offered is
// serverSignatureBlobs' second return: the number of shape-valid candidates a
// server offered, counted before that function's own copy was capped at
// helpers.MaxSignaturesPerCollection. The two are combined here at their true,
// pre-truncation size, which is what serverSignatureBlobs' own doc comment
// means by leaving the truncation decision to this function: a server list
// alone crossing the cap has to be observable from offered even though the
// server slice gatherOne indexes into is already capped.
//
// That asymmetry - limit is computed from a pre-truncation count while the
// slice gatherOne reads server candidates from is a post-truncation one - is
// worth an index-safety argument rather than a shape a reader takes on faith.
// gatherOne's maximum server index is limit-1-len(sources). When
// offered <= helpers.MaxSignaturesPerCollection, serverSignatureBlobs never
// truncated, so len(server) == offered and limit <= sources+offered =
// sources+len(server). When offered > helpers.MaxSignaturesPerCollection,
// serverSignatureBlobs' own guard truncated at the cap, so
// len(server) == helpers.MaxSignaturesPerCollection and
// limit <= helpers.MaxSignaturesPerCollection = len(server). Both cases give
// limit <= len(sources)+len(server), which is exactly the bound gatherOne's
// index needs: no out-of-range read is reachable either way.
func gatherLimit(runtime *infra.Infra, col collection, sources, offered int) int {
	limit := min(sources+offered, helpers.MaxSignaturesPerCollection)
	if dropped := sources + offered - limit; dropped > 0 {
		runtime.Output.Warnf(
			"%s: %d signature candidates exceed the limit of %d (%d declared, %d offered by the server); "+
				"the last %d were not gathered, and declared sources always come first",
			col.key(), sources+offered, helpers.MaxSignaturesPerCollection, sources, offered, dropped)
	}

	return limit
}

// gatherOne produces the i-th blob of the concatenated gather: a requirement's
// own source is fetched, a server's own signature is already in hand.
//
// Fetching through Fetcher.FetchRequirementSource is what binds a repository's
// source to the client that carries no Galaxy token and no relaxed TLS policy;
// see that method's own doc comment for why the provenance rule is stated on
// the method rather than carried by a type.
func (vc *verifyContext) gatherOne(
	ctx context.Context,
	sources []string,
	server []signature.Blob,
	i int,
) (signature.Blob, error) {
	if i < len(sources) {
		return vc.fetcher.FetchRequirementSource(ctx, sources[i])
	}

	return server[i-len(sources)], nil
}

// serverSignatureBlobs reads the signatures a Galaxy server carried in its own
// version metadata for a collection.
//
// The hub type declares that field as `any`, so the shape is decided here: a
// list of objects, each carrying a string under "signature". Anything else is
// skipped without an error - a third-party server may send a shape this tool
// does not read, and refusing the whole install over it would make one server's
// spelling a hard failure rather than a signature this run does not have. A
// blob over helpers.SignatureMaxSize is dropped the same way, which is the same
// ceiling Fetcher applies to a source it fetches.
//
// The copied slice is bounded at helpers.MaxSignaturesPerCollection entries;
// the offered count returned alongside it deliberately is not. The make
// capacity below and the len(blobs) < helpers.MaxSignaturesPerCollection guard
// around the append together keep at most that many blobs ever materialized,
// whatever the list's own length, while offered counts every shape-valid entry
// regardless of whether it was copied. That split matters because gatherLimit
// combines this count with a requirements entry's own declared sources and
// needs the true, pre-truncation number to decide whether the combined set
// actually exceeded the cap: a caller handed a post-truncation count instead
// could never observe a server list alone crossing it, since that count would
// already read as "at most 64" no matter how many the server actually offered.
// Measured at the MetadataMaxSize ceiling (932,062 entries, in an
// already-resident 324.4 MiB decoded document): the capped copy is 1,048 B
// against the 42.7 MiB an uncapped one would be, with at most 64 blobs ever
// indexed regardless of how many are offered; the counting walk itself costs
// 25.2 ms against 10.3 us for an early break that stops counting at the cap,
// allocations unchanged at 65.
//
// Origin names the server's own metadata document rather than any part of the
// blob, since a Failure reports it back verbatim and what an operator needs
// there is which source produced the signature.
//
// It returns (nil, 0) and allocates nothing for the overwhelmingly common
// shapes - a nil meta, or a server that sent no signatures - because it is a
// shape filter rather than a validator: what a gathered blob is worth is
// signature.Verify's verdict, not this function's.
func serverSignatureBlobs(meta *types.GalaxyCollectionVersionInfo) ([]signature.Blob, int) {
	if meta == nil {
		return nil, 0
	}
	list, ok := meta.Signatures.([]any)
	if !ok || len(list) == 0 {
		return nil, 0
	}

	origin := serverBlobOrigin(meta)
	offered := 0
	blobs := make([]signature.Blob, 0, min(len(list), helpers.MaxSignaturesPerCollection))
	for _, entry := range list {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		value, ok := row[serverSignatureField].(string)
		if !ok || value == "" || int64(len(value)) > helpers.SignatureMaxSize {
			continue
		}
		offered++
		if len(blobs) < helpers.MaxSignaturesPerCollection {
			blobs = append(blobs, signature.Blob{Origin: origin, Data: []byte(value)})
		}
	}

	return blobs, offered
}

// serverBlobOrigin names where a server-carried signature came from: the
// version-metadata document's own URL when the server stated one, and a fixed
// label when it did not, so a Failure never reports an empty origin.
//
// The href is bounded here, at the producer, rather than where it is rendered.
// It is a server- and snapshot-chosen string capped only by
// helpers.MetadataMaxSize, it is copied onto every blob gathered for the
// collection, and signature.verificationError renders it with %q once per
// non-ignored failure - so an 8 MiB href across a full
// helpers.MaxSignaturesPerCollection set produced a 512.0 MiB error string,
// measured, materialized twice per collection - the collection-key wrap this
// file's own verifyCollectionSignatures builds and the worker's own
// "Failed: ..." line - and written to stderr once, times cfg.Workers.
// Bounding it once, where the value enters this program's own vocabulary, is
// what keeps every consumer of Blob.Origin out of that multiplication.
//
// The credential cut is applied at the same place and for the same reason: a
// version-metadata href is a server-supplied URL this run renders rather than
// requests, so it goes through helpers.WithoutCredentials like every other such
// sink. The order is load-bearing - cut, then truncate - since truncating
// first would leave whatever fits inside the cap, credential included, and the
// cut would then have nothing to find beyond it.
func serverBlobOrigin(meta *types.GalaxyCollectionVersionInfo) string {
	if meta.Href != "" {
		return helpers.TruncateForMessage(helpers.WithoutCredentials(meta.Href))
	}

	return "galaxy server version metadata"
}

// signatureDeadlineError normalizes err into helpers.ErrSignatureFetchDeadline
// when, and only when, this collection's own signature budget (sigCtx, built
// from context.WithTimeout(parent, budget)) is what ended the work - the same
// four-argument shape, and the same two ambient checks, as artifactDeadlineError
// (internal/galaxy/collections/deadline.go). A live parent whose own context
// ended first, or a sigCtx that is still live, leaves err untouched.
//
// It carries no content-based exclusion arm, unlike artifactDeadlineError's own
// helpers.ErrSHA256Mismatch carve-out, and the reason is what this surface can
// produce rather than a difference of policy. When sigCtx expires, the only
// context-observing call in the gather is Fetcher.fetchHTTP, so the error
// arrives as helpers.ErrSignatureSourceUnavailable wrapping the context cause -
// which is precisely the value that sentinel's own doc comment says to relabel,
// and both classify ExitNetwork, so no exit code moves. A verification verdict
// and a spent budget are mutually exclusive for a reason of shape rather than
// of ordering: signature.Verify reaches a verdict only where the pull returned
// no error, whether it ran out or stopped early at the Count-th distinct signer,
// and a pull whose fetch was ended by the budget returns that error instead.
//
// It is idempotent for the same reason artifactDeadlineError is, so a caller
// may normalize an error that already carries the sentinel without doubling it.
//
// The cause is rendered with %v, deliberately never %w. Three places enforce
// that rule for the five deadline-and-stall sentinels this project raises, and
// this is the third: internal/galaxy/fetch's watchdogBody.Read together with
// artifactDeadlineError (internal/galaxy/collections/deadline.go),
// internal/galaxy/cache's deadlineError for the metadata and state-object
// budgets, and this one. helpers.ErrSignatureFetchDeadline's own doc comment
// holds the argument: leaving context.Canceled reachable through errors.Is
// would have exitcode.FromError report a hostile or degraded signature host as
// a caught Ctrl-C.
func signatureDeadlineError(parent, sigCtx context.Context, budget time.Duration, err error) error {
	if err == nil || errors.Is(err, helpers.ErrSignatureFetchDeadline) {
		return err
	}
	if parent.Err() != nil || !errors.Is(sigCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and helpers.ErrSignatureFetchDeadline's own.
	return fmt.Errorf("%w after %s: %v", helpers.ErrSignatureFetchDeadline, budget, err)
}
