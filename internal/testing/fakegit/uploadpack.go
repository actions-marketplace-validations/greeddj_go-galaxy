package fakegit

import (
	"bytes"
	"io"
	"slices"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Wire constants of the upload-pack exchange this package speaks: the
// service name the smart-HTTP prefix line and the ssh exec command carry,
// the agent string the advertisement names, the depth this fake honors and
// the delta window the pack encoder is run with (go-git's own server uses
// the same window).
const (
	uploadPackService = "git-upload-pack"
	agentValue        = "fakegit"
	honoredDepth      = packp.DepthCommits(1)
	packWindow        = 10
	donePayload       = "done"
)

// uploadPackReply is the pre-rendered answer to one upload-pack request,
// split where the transports need it split. header is everything before
// the packfile - the shallow update when the request carried a depth, then
// the NAK - or, when the request was refused, the ERR pkt-line that stands
// in for both. pack is the packfile itself, kept apart so a StallAfterBytes
// fault counts pack bytes alone. badRequest is non-empty when the request
// is malformed in a way this fake answers with a protocol-level failure
// rather than an ERR line (a deepen this fake does not honor): HTTP turns it
// into a 400, ssh into a stderr line and a non-zero exit. Rendering the
// whole reply before writing any of it means no encoder goroutine outlives
// the handler, and a fault that blocks mid-pack blocks with the repository
// lock already released.
type uploadPackReply struct {
	badRequest string
	header     []byte
	pack       []byte
}

// advertise writes the reference advertisement for repo with caps to w.
// withServicePrefix adds the "# service=git-upload-pack" line and the flush
// that smart HTTP puts in front of the advertisement; ssh has no such
// prefix. The advertised capabilities are agent, ofs-delta and no-progress,
// plus shallow and allow-reachable-sha1-in-want when caps asks for them, and
// symref=HEAD:<branch> when HEAD is symbolic. Neither side-band nor
// multi_ack nor thin-pack is ever advertised: without side-band the pack
// follows the NAK raw, which keeps a stall a plain byte count.
func advertise(w io.Writer, repo *Repo, caps Capabilities, withServicePrefix bool) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	ar := packp.NewAdvRefs()
	if withServicePrefix {
		ar.Prefix = [][]byte{[]byte("# service=" + uploadPackService), pktline.Flush}
	}
	// capability.List.Set only fails for a capability that takes no argument
	// being given one, or the reverse; these calls are spelled correctly by
	// construction, so their errors are asserted away.
	_ = ar.Capabilities.Set(capability.Agent, agentValue)
	_ = ar.Capabilities.Set(capability.OFSDelta)
	_ = ar.Capabilities.Set(capability.NoProgress)
	if caps.Shallow {
		_ = ar.Capabilities.Set(capability.Shallow)
	}
	if caps.AllowReachableSHA1 {
		_ = ar.Capabilities.Set(capability.AllowReachableSHA1InWant)
	}

	if err := addReferences(repo, ar); err != nil {
		return err
	}
	if err := addHead(repo, ar); err != nil {
		return err
	}
	return ar.Encode(w)
}

// addReferences lists every hash reference under refs/ in ar, with a
// peeled entry for each one that names a tag object. The caller holds
// repo.mu.
func addReferences(repo *Repo, ar *packp.AdvRefs) error {
	iter, err := repo.st.IterReferences()
	if err != nil {
		return err
	}
	return iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || ref.Name() == plumbing.HEAD {
			return nil
		}
		ar.References[ref.Name().String()] = ref.Hash()
		if tag, tagErr := object.GetTag(repo.st, ref.Hash()); tagErr == nil {
			ar.Peeled[ref.Name().String()] = tag.Target
		}
		return nil
	})
}

// addHead sets ar.Head to the commit HEAD resolves to and, when HEAD is
// symbolic, records the symref capability for it. A HEAD that names an
// unborn branch advertises nothing, as git does. The caller holds repo.mu.
func addHead(repo *Repo, ar *packp.AdvRefs) error {
	ref, err := repo.st.Reference(plumbing.HEAD)
	if err != nil {
		// No HEAD at all is an empty advertisement, not a failure.
		return nil
	}
	if ref.Type() == plumbing.SymbolicReference {
		if err := ar.AddReference(ref); err != nil {
			return err
		}
		ref, err = storer.ResolveReference(repo.st, ref.Target())
		if err != nil {
			// An unborn branch: HEAD exists but resolves to nothing yet.
			return nil
		}
	}
	h := ref.Hash()
	ar.Head = &h
	return nil
}

// readUploadRequest reads one upload-pack request off r: the upload-request
// proper (wants, shallows, deepen, flush), then whatever haves follow, up to
// and including the "done" line, or EOF. It returns the decoded request, or
// closed=true when the client opened the exchange with a flush - what go-git
// sends over ssh to end a session after only reading the advertisement.
// The request is re-encoded into a buffer before decoding because
// packp.UploadRequest.Decode stops at the flush and the transport needs the
// body drained to the "done" line regardless.
func readUploadRequest(r io.Reader) (*packp.UploadRequest, bool, error) {
	buf, closed, err := drainRequest(r)
	if err != nil || closed {
		return nil, closed, err
	}
	req := packp.NewUploadRequest()
	if decErr := req.Decode(buf); decErr != nil {
		return nil, false, decErr
	}
	return req, false, nil
}

// drainRequest copies pkt-lines from r into a buffer through the "done"
// line or EOF. It reports closed when the first line is a flush, or there
// is no line at all: the client's way of ending a session without a request.
func drainRequest(r io.Reader) (*bytes.Buffer, bool, error) {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	sc := pktline.NewScanner(r)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if first && len(line) == 0 {
			return nil, true, nil
		}
		first = false
		var encErr error
		if len(line) == 0 {
			encErr = enc.Flush()
		} else {
			encErr = enc.Encode(line)
		}
		if encErr != nil {
			return nil, false, encErr
		}
		if string(bytes.TrimSuffix(line, []byte("\n"))) == donePayload {
			break
		}
	}
	if scanErr := sc.Err(); scanErr != nil {
		return nil, false, scanErr
	}
	return &buf, first, nil
}

// buildReply renders the answer to req against repo with caps, under the
// repository lock. fault is the matched upload-pack fault, if any; only its
// ServeCommit field is read here, the transports enact the rest.
//
// Wants are checked against the advertised tips (every reference hash and
// HEAD, but not a peeled "^{}" hash: git marks only the ref hashes as its
// own and answers a want for a peeled commit with "not our ref"); a want that
// is none of those is refused with an ERR line unless caps.AllowReachableSHA1
// is set and the want is reachable from a tip (membership of closure over the
// tips), which is how a real server treats allow-reachable-sha1-in-want. A depth is honored only as
// DepthCommits(1) for a single want, and only when caps.Shallow is set: a
// deepen against a server that did not advertise shallow, a deeper depth, or
// several wants with a depth are a badRequest, so that a client deciding its
// depth from anything but the advertisement fails loudly rather than being
// quietly served a full pack.
func buildReply(repo *Repo, caps Capabilities, req *packp.UploadRequest, fault Fault) (uploadPackReply, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	tips, err := advertisedTips(repo)
	if err != nil {
		return uploadPackReply{}, err
	}
	refused, err := refusedWant(repo, caps, req.Wants, tips)
	if err != nil {
		return uploadPackReply{}, err
	}
	if refused != plumbing.ZeroHash {
		return refusalReply(refused)
	}

	wants := req.Wants
	if fault.ServeCommit != plumbing.ZeroHash {
		wants = []plumbing.Hash{fault.ServeCommit}
	}
	objs, shallows, badRequest, err := selectObjects(repo, caps, req.Depth, wants)
	if err != nil || badRequest != "" {
		return uploadPackReply{badRequest: badRequest}, err
	}
	return renderReply(repo, !req.Depth.IsZero(), shallows, objs)
}

// refusalReply renders the ERR line that answers a want this server will
// not serve, in place of any NAK or pack.
func refusalReply(refused plumbing.Hash) (uploadPackReply, error) {
	var hdr bytes.Buffer
	if err := pktline.NewEncoder(&hdr).Encodef("ERR upload-pack: not our ref %s\n", refused); err != nil {
		return uploadPackReply{}, err
	}
	return uploadPackReply{header: hdr.Bytes()}, nil
}

// selectObjects decides what the pack holds for wants at depth, returning
// the objects, the shallow commits and a badRequest reason: the full closure
// when there is no depth, the single-commit shape of shallowObjects for the
// one depth this fake honors, or a reason for any other depth. The caller
// holds repo.mu.
func selectObjects(repo *Repo, caps Capabilities, depth packp.Depth, wants []plumbing.Hash) (
	[]plumbing.Hash, []plumbing.Hash, string, error,
) {
	switch {
	case depth.IsZero():
		objs, err := closure(repo.st, wants)
		return objs, nil, "", err
	case !caps.Shallow:
		return nil, nil, "deepen requested but shallow was not advertised", nil
	case depth != honoredDepth || len(wants) != 1:
		return nil, nil, "only deepen 1 with a single want is served", nil
	default:
		objs, shallows, err := shallowObjects(repo, wants[0])
		return objs, shallows, "", err
	}
}

// renderReply encodes the header (the shallow update when the request was
// shallow, then the NAK) and the packfile over objs. The caller holds
// repo.mu.
func renderReply(repo *Repo, shallow bool, shallows, objs []plumbing.Hash) (uploadPackReply, error) {
	var hdr bytes.Buffer
	if shallow {
		su := packp.ShallowUpdate{Shallows: shallows}
		if err := su.Encode(&hdr); err != nil {
			return uploadPackReply{}, err
		}
	}
	var nak packp.ServerResponse
	if err := nak.Encode(&hdr, false); err != nil {
		return uploadPackReply{}, err
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, repo.st, false).Encode(objs, packWindow); err != nil {
		return uploadPackReply{}, err
	}
	return uploadPackReply{header: hdr.Bytes(), pack: pack.Bytes()}, nil
}

// advertisedTips collects every hash git's upload-pack would accept as a
// want without allow-*-sha1-in-want: reference hashes and HEAD. A peeled tag
// target is advertised but is not a tip, exactly as on a real server. The
// caller holds repo.mu.
func advertisedTips(repo *Repo) ([]plumbing.Hash, error) {
	ar := packp.NewAdvRefs()
	if err := addReferences(repo, ar); err != nil {
		return nil, err
	}
	if err := addHead(repo, ar); err != nil {
		return nil, err
	}
	tips := make([]plumbing.Hash, 0, len(ar.References)+1)
	for _, h := range ar.References {
		tips = append(tips, h)
	}
	if ar.Head != nil {
		tips = append(tips, *ar.Head)
	}
	return tips, nil
}

// refusedWant returns the first want that is not a tip and - when
// caps.AllowReachableSHA1 is set - not reachable from one either, or
// plumbing.ZeroHash when every want is acceptable. Reachability is the
// membership of closure over all tips, computed only when a non-tip want is
// actually present. The caller holds repo.mu.
func refusedWant(repo *Repo, caps Capabilities, wants, tips []plumbing.Hash) (plumbing.Hash, error) {
	var reachable []plumbing.Hash
	reachableKnown := false
	for _, w := range wants {
		if slices.Contains(tips, w) {
			continue
		}
		if !caps.AllowReachableSHA1 {
			return w, nil
		}
		if !reachableKnown {
			var err error
			reachable, err = closure(repo.st, tips)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			reachableKnown = true
		}
		if !slices.Contains(reachable, w) {
			return w, nil
		}
	}
	return plumbing.ZeroHash, nil
}

// shallowObjects lists what a depth-1 fetch of want ships: the tag object
// when want names one, the commit it peels to, and that commit's tree
// closure - nothing from its parents. The returned shallows carry that one
// commit, which is what the shallow update then reports. The caller holds
// repo.mu.
func shallowObjects(repo *Repo, want plumbing.Hash) ([]plumbing.Hash, []plumbing.Hash, error) {
	var objs []plumbing.Hash
	commitHash := want
	if tag, tagErr := object.GetTag(repo.st, want); tagErr == nil {
		objs = append(objs, want)
		commitHash = tag.Target
	}
	commit, err := object.GetCommit(repo.st, commitHash)
	if err != nil {
		return nil, nil, err
	}
	treeObjs, err := closure(repo.st, []plumbing.Hash{commit.TreeHash})
	if err != nil {
		return nil, nil, err
	}
	objs = append(objs, commitHash)
	objs = append(objs, treeObjs...)
	return objs, []plumbing.Hash{commitHash}, nil
}

// closure lists every object reachable from roots: commits with their
// parents and trees, tag objects with their targets, trees with their
// entries, and blobs. It is this package's own walk rather than
// revlist.Objects because go-git's tree walker refuses an entry named ".",
// "..", ".git" or carrying a control character - the very entries
// RawTreeCommit exists to ship, so a server that walked through go-git could
// never serve them. A submodule entry is skipped, as git skips it: the commit
// it names lives in another repository. Each object is listed once, in
// discovery order.
func closure(st storer.EncodedObjectStorer, roots []plumbing.Hash) ([]plumbing.Hash, error) {
	seen := make(map[plumbing.Hash]bool)
	var out []plumbing.Hash
	pending := slices.Clone(roots)
	for len(pending) > 0 {
		h := pending[0]
		pending = pending[1:]
		if seen[h] {
			continue
		}
		seen[h] = true
		obj, err := st.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
		next, err := children(st, obj)
		if err != nil {
			return nil, err
		}
		pending = append(pending, next...)
	}
	return out, nil
}

// children lists the objects obj refers to directly; see closure.
func children(st storer.EncodedObjectStorer, obj plumbing.EncodedObject) ([]plumbing.Hash, error) {
	decoded, err := object.DecodeObject(st, obj)
	if err != nil {
		return nil, err
	}
	switch o := decoded.(type) {
	case *object.Commit:
		return append([]plumbing.Hash{o.TreeHash}, o.ParentHashes...), nil
	case *object.Tag:
		return []plumbing.Hash{o.Target}, nil
	case *object.Tree:
		var next []plumbing.Hash
		for _, e := range o.Entries {
			if e.Mode != filemode.Submodule {
				next = append(next, e.Hash)
			}
		}
		return next, nil
	default:
		return nil, nil
	}
}
