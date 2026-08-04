package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dostos/relay/internal/bridge"
)

// AuthorizeBridgeRequest is the single policy boundary for an authenticated
// desktop-control invocation. Services below this boundary retain structural
// invariants (cycles, state transitions and storage role), but do not decide
// whether a valid bridge identity may exercise ordinary hierarchy authority.
func AuthorizeBridgeRequest(source bridge.Source, argv []string) error {
	if source.SessionID == HomeClientSessionID {
		args := authorityArgs(argv)
		if len(args) == 0 {
			return fmt.Errorf("missing operation")
		}
		digest := authorityDigest(source.SessionID, args, source.RequestID)
		allowed, reason, err := appendAuthorityDecision(digest, source.SessionID, strings.Join(args, " "), true, "authenticated local human request")
		if err != nil {
			return fmt.Errorf("record authority decision: %w", err)
		}
		if !allowed {
			return fmt.Errorf("authenticated authority refused: %s", reason)
		}
		return nil
	}
	actor, err := (&Registry{}).GetSession(source.SessionID)
	if err != nil {
		return err
	}
	args := authorityArgs(argv)
	operation := strings.Join(args, " ")
	allowed, reason := authorizeOperation(&Registry{}, actor, args)
	digest := authorityDigest(actor.ID, args, source.RequestID)
	recordedAllowed, recordedReason, receiptErr := appendAuthorityDecision(digest, actor.ID, operation, allowed, reason)
	if receiptErr != nil {
		return fmt.Errorf("record authority decision: %w", receiptErr)
	}
	if !recordedAllowed {
		return fmt.Errorf("authenticated authority refused: %s", recordedReason)
	}
	return nil
}

func authorityArgs(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg != "--json" {
			out = append(out, arg)
		}
	}
	return out
}

type authorityOperationKind string

const (
	authorityInvalid       authorityOperationKind = "invalid"
	authorityHumanRequired authorityOperationKind = "human_required"
	authorityDiscovery     authorityOperationKind = "discovery"
	authorityStart         authorityOperationKind = "start"
	authorityLifecycle     authorityOperationKind = "lifecycle"
	authorityHandoffTarget authorityOperationKind = "handoff_target"
	authoritySessionTarget authorityOperationKind = "session_target"
	authorityMessageTarget authorityOperationKind = "message_target"
	authorityParentTarget  authorityOperationKind = "parent_target"
	authorityRoot          authorityOperationKind = "root"
)

// authorityOperation is the typed policy input. Command parsing belongs here,
// at the authenticated boundary; downstream services enforce only structural
// invariants and never infer authority from an argv position.
type authorityOperation struct {
	Kind        authorityOperationKind
	Verb        string
	Target      string
	Host        string
	Scope       string
	RemoteScope string
	Args        []string
}

func parseAuthorityOperation(args []string) authorityOperation {
	op := authorityOperation{Kind: authorityInvalid, Args: args}
	if len(args) == 0 {
		return op
	}
	top := args[0]
	sub := ""
	if len(args) > 1 {
		sub = args[1]
	}
	op.Verb = strings.TrimSpace(top + " " + sub)

	if top == "auth" || top == "host" || top == "policy" || top == "install-cmux-restore" ||
		(top == "root" && sub == "control-plane") || (top == "viz" && sub == "retire-control") {
		op.Kind = authorityHumanRequired
		return op
	}
	if top == "root" {
		op.Kind = authorityRoot
		return op
	}

	switch top {
	case "agent":
		switch sub {
		case "protocol", "help", "--help", "-h", "pick":
			op.Kind = authorityDiscovery
		case "start":
			op.Kind = authorityStart
			if len(args) > 2 {
				op.Host = args[2]
			}
			op.Target = flagValue(args[2:], "--parent")
		case "restart", "wait", "send", "capture", "done", "status":
			op.Kind = authorityHandoffTarget
			if len(args) > 2 {
				op.Target = args[2]
			}
		}
	case "handoff":
		switch sub {
		case "list":
			op.Kind = authorityDiscovery
		case "get", "finalize":
			op.Kind = authorityHandoffTarget
			if len(args) > 2 {
				op.Target = args[2]
			}
		case "reconcile":
			op.Kind = authorityLifecycle
		default:
			// The legacy `relay handoff -H ...` form starts work.
			op.Kind = authorityStart
			op.Host = firstFlagValue(args[1:], "-H", "--host")
			op.Target = flagValue(args[1:], "--parent")
		}
	case "session", "sess":
		switch sub {
		case "list":
			op.Kind = authorityDiscovery
		case "create", "adopt":
			op.Kind = authorityStart
			op.Host = firstFlagValue(args[2:], "-H", "--host")
		case "get", "rename", "bridge", "capture", "send", "exec", "resize", "attach", "destroy", "cleanup":
			op.Kind = authoritySessionTarget
			if len(args) > 2 {
				op.Target = args[2]
			}
		}
	case "parent":
		op.Kind = authorityParentTarget
	case "resolve":
		op.Kind = authorityMessageTarget
		if len(args) > 1 {
			op.Target = args[1]
		}
	case "targets", "doctor", "history", "log", "help", "version", "build":
		op.Kind = authorityDiscovery
	case "client":
		switch sub {
		case "list", "status", "update-status", "--help", "-h":
			op.Kind = authorityDiscovery
		case "update":
			// The client owns the update repository, remote, and branch policy.
			// Home only appends a durable lifecycle request to an enrolled
			// channel; it does not reinterpret that policy or execute remotely.
			op.Kind = authorityLifecycle
		}
	case "resume":
		if sub == "list" {
			op.Kind = authorityDiscovery
		} else {
			op.Kind = authorityLifecycle
		}
	case "signal", "hook", "ask", "supervise", "gc", "events":
		op.Kind = authorityLifecycle
	case "msg", "board", "viz", "pane":
		op.Kind = authorityLifecycle
	}
	if op.Kind == authorityStart {
		op.Scope = flagValue(args[1:], "--repo")
		op.RemoteScope = firstFlagValue(args[1:], "--cwd", "-R")
	}
	return op
}

func firstFlagValue(args []string, names ...string) string {
	for _, name := range names {
		if value := flagValue(args, name); value != "" {
			return value
		}
	}
	return ""
}

func flagValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func authorizeOperation(reg *Registry, actor *Session, args []string) (bool, string) {
	if actor == nil || len(args) == 0 {
		return false, "missing actor or operation"
	}
	op := parseAuthorityOperation(args)
	if op.Kind == authorityHumanRequired {
		return false, "external trust, credential, security, or human policy operation"
	}
	if op.Kind == authorityRoot {
		if len(args) < 2 {
			return false, "root subcommand required"
		}
		apex, apexErr := (&RootService{Reg: reg}).Apex()
		if args[1] == "adopt" && apexErr != nil && len(args) == 3 && args[2] == actor.ID && actor.SourceSessionID == "" {
			return true, "authenticated root self-adoption"
		}
		if apexErr != nil || apex.ID != actor.ID {
			return false, "caller is not the governing apex"
		}
		return true, "governing apex authority"
	}
	if op.Kind == authorityParentTarget && len(args) >= 2 && (args[1] == "link" || args[1] == "move" || args[1] == "reparent") {
		if len(args) != 4 {
			return false, "parent mutation requires parent and handoff"
		}
		parentID, handoffID := args[2], args[3]
		ho, err := reg.GetHandoff(handoffID)
		if err != nil {
			return false, "handoff not found"
		}
		if actor.ID == ho.SourceSessionID || sessionAncestor(reg, actor.ID, ho.SourceSessionID) || (ho.SourceSessionID == "" && actor.ID == parentID) {
			return true, "manager lineage authority"
		}
		return false, "target is outside caller lineage"
	}
	if op.Kind == authorityParentTarget {
		if len(args) < 2 {
			return false, "parent subcommand required"
		}
		// Commands naming a manager session are confined to that manager or a
		// governing ancestor. Message commands resolve their durable inbox owner.
		if map[string]bool{"inbox": true, "log": true, "sweep": true, "status": true, "retire": true, "state": true, "active": true, "idle": true, "complete": true}[args[1]] {
			if len(args) < 3 || !(actor.ID == args[2] || sessionAncestor(reg, actor.ID, args[2])) {
				return false, "parent target is outside caller lineage"
			}
			return true, "manager lineage authority"
		}
		if map[string]bool{"reply": true, "ack": true, "redeliver": true}[args[1]] && len(args) >= 3 {
			msg, err := (&ParentService{Reg: reg}).FindMessage(args[2])
			if err != nil || !(actor.ID == msg.ParentSessionID || sessionAncestor(reg, actor.ID, msg.ParentSessionID)) {
				return false, "message is outside caller lineage"
			}
			return true, "manager message authority"
		}
		if args[1] == "send" || args[1] == "watch" {
			return true, "authenticated manager operation"
		}
		return false, "parent operation is outside authenticated scope"
	}
	if op.Kind == authorityMessageTarget && op.Target != "" {
		msg, err := (&ParentService{Reg: reg}).FindMessage(op.Target)
		if err != nil || !(actor.ID == msg.ParentSessionID || sessionAncestor(reg, actor.ID, msg.ParentSessionID)) {
			return false, "message is outside caller lineage"
		}
		return true, "manager message authority"
	}
	if op.Kind == authorityHandoffTarget {
		if op.Target == "" {
			return false, "handoff target required"
		}
		ho, err := reg.GetHandoff(op.Target)
		if err != nil || !(actor.ID == ho.SourceSessionID || sessionAncestor(reg, actor.ID, ho.SourceSessionID)) {
			return false, "handoff is outside caller lineage"
		}
		return true, "manager handoff authority"
	}
	if op.Kind == authoritySessionTarget {
		if op.Target == "" {
			return false, "session target required"
		}
		target, err := reg.GetSession(op.Target)
		if err != nil {
			return false, "session target not found"
		}
		if actor.ID == target.ID || sessionAncestor(reg, actor.ID, target.ID) {
			return true, "manager session authority"
		}
		return false, "session target is outside caller lineage"
	}
	if op.Kind == authorityDiscovery {
		return true, "authenticated discovery"
	}
	if op.Kind == authorityStart {
		if op.Target != "" && !(op.Target == actor.ID || sessionAncestor(reg, actor.ID, op.Target)) {
			return false, "requested parent is outside caller lineage"
		}
		if op.Scope != "" && !sessionDeclaresRepoScope(actor, op.Scope) {
			return false, "requested repository is outside caller declared scope"
		}
		if op.RemoteScope != "" && !sessionDeclaresRemoteScope(actor, op.Host, op.RemoteScope) {
			return false, "requested remote working directory is outside caller declared scope"
		}
		return true, "authenticated child start"
	}
	if op.Kind == authorityLifecycle {
		return true, "authenticated lifecycle operation"
	}
	return false, "operation is outside authenticated Relay scope"
}

func sessionDeclaresRemoteScope(actor *Session, host, requested string) bool {
	if actor == nil || strings.TrimSpace(requested) == "" || strings.TrimSpace(host) == "" || host != actor.HostID {
		return false
	}
	root := expandAuthorityHome(actor.RemoteCWD)
	cleanRequested := expandAuthorityHome(requested)
	if root == "" || cleanRequested == "" || !filepath.IsAbs(root) || !filepath.IsAbs(cleanRequested) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(cleanRequested))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func expandAuthorityHome(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func sessionDeclaresRepoScope(actor *Session, requested string) bool {
	if actor == nil || strings.TrimSpace(requested) == "" {
		return false
	}
	roots := append([]string{}, actor.RepoRefs...)
	if actor.RepoRef != "" {
		roots = append(roots, actor.RepoRef)
	}
	cleanRequested := filepath.Clean(requested)
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || !filepath.IsAbs(root) || !filepath.IsAbs(cleanRequested) {
			continue
		}
		rel, err := filepath.Rel(root, cleanRequested)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func sessionAncestor(reg *Registry, ancestorID, sessionID string) bool {
	if ancestorID == "" || sessionID == "" {
		return false
	}
	if ancestorID == sessionID {
		return true
	}
	for _, ancestor := range AncestorChain(reg, sessionID) {
		if ancestor.ID == ancestorID {
			return true
		}
	}
	return false
}

func authorityDigest(actor string, args []string, requestID string) string {
	identity := "legacy:" + strings.Join(args, "\x00")
	// Protocol v2 request identity distinguishes separate invocations with the
	// same argv while keeping retries and restart replay idempotent.
	if requestID != "" {
		identity = "request:" + requestID
	}
	sum := sha256.Sum256([]byte(actor + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

type authorityReceiptIndex struct {
	V        int    `json:"v"`
	Digest   string `json:"request_digest"`
	Receipt  string `json:"receipt_id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

const maxAuthorityReceiptIndexBytes = 64 << 10

func authorityReceiptIndexPath(digest string) string {
	return filepath.Join(AuthorityReceiptIndexDir(), digest+".json")
}

func readAuthorityReceiptIndex(digest string) (*authorityReceiptIndex, error) {
	path := authorityReceiptIndexPath(digest)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxAuthorityReceiptIndexBytes {
		return nil, fmt.Errorf("unsafe or oversized authority receipt index for %s", digest)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var marker authorityReceiptIndex
	if err := json.Unmarshal(raw, &marker); err != nil {
		return nil, fmt.Errorf("corrupt authority receipt index for %s: %w", digest, err)
	}
	if marker.V != 1 || marker.Digest != digest || marker.Receipt != "ar-"+digest[:16] || (marker.Decision != "allow" && marker.Decision != "deny") {
		return nil, fmt.Errorf("corrupt authority receipt index for %s", digest)
	}
	return &marker, nil
}

func writeAuthorityReceiptIndex(digest string, allowed bool, reason string) error {
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	marker := authorityReceiptIndex{V: 1, Digest: digest, Receipt: "ar-" + digest[:16], Decision: decision, Reason: reason}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(AuthorityReceiptIndexDir(), ".receipt-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, authorityReceiptIndexPath(digest)); err != nil {
		return err
	}
	dir, err := os.Open(AuthorityReceiptIndexDir())
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ledgerHasAuthorityDecision is used only to lazily migrate receipts written
// before the compact index existed. json.Decoder has no Scanner token ceiling,
// so an unrelated large goal/command record cannot disable authorization.
func ledgerAuthorityDecision(digest string) (bool, bool, string, error) {
	file, err := os.Open(LedgerPath())
	if os.IsNotExist(err) {
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for recordNumber := 1; ; recordNumber++ {
		var record struct {
			Type     string `json:"type"`
			Digest   string `json:"request_digest"`
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			return false, false, "", nil
		} else if err != nil {
			return false, false, "", fmt.Errorf("decode authority ledger record %d: %w", recordNumber, err)
		}
		if record.Type == "authority_decision" && record.Digest == digest {
			if record.Decision != "allow" && record.Decision != "deny" {
				return false, false, "", fmt.Errorf("authority ledger record %d has invalid decision", recordNumber)
			}
			return true, record.Decision == "allow", record.Reason, nil
		}
	}
}

// appendAuthorityDecision makes repeated bridge delivery idempotent. The
// authority lock covers index lookup, legacy receipt discovery, ledger append,
// and index publication, so concurrent retries yield one durable audit record.
func appendAuthorityDecision(digest, actor, operation string, allowed bool, reason string) (bool, string, error) {
	if err := EnsureAuthorityWritable(); err != nil {
		return false, "", err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return false, "", err
	}
	defer unlock()
	indexed, err := readAuthorityReceiptIndex(digest)
	if err != nil {
		return false, "", err
	}
	if indexed != nil {
		return indexed.Decision == "allow", indexed.Reason, nil
	}
	legacy, legacyAllowed, legacyReason, err := ledgerAuthorityDecision(digest)
	if err != nil {
		return false, "", err
	}
	if legacy {
		if err := writeAuthorityReceiptIndex(digest, legacyAllowed, legacyReason); err != nil {
			return false, "", err
		}
		return legacyAllowed, legacyReason, nil
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	if err := appendLedgerLocked(map[string]any{
		"v": 1, "type": "authority_decision", "ts": time.Now().UTC().Format(time.RFC3339Nano),
		"receipt_id": "ar-" + digest[:16], "request_digest": digest,
		"actor_session_id": actor, "operation": operation, "decision": decision, "reason": reason,
	}); err != nil {
		return false, "", err
	}
	if err := writeAuthorityReceiptIndex(digest, allowed, reason); err != nil {
		return false, "", err
	}
	return allowed, reason, nil
}
