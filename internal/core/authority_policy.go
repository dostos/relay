package core

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dostos/relay/internal/bridge"
)

// AuthorizeBridgeRequest is the single policy boundary for an authenticated
// desktop-control invocation. Services below this boundary retain structural
// invariants (cycles, state transitions and storage role), but do not decide
// whether a valid bridge identity may exercise ordinary hierarchy authority.
func AuthorizeBridgeRequest(source bridge.Source, argv []string) error {
	actor, err := (&Registry{}).GetSession(source.SessionID)
	if err != nil {
		return err
	}
	args := authorityArgs(argv)
	operation := strings.Join(args, " ")
	allowed, reason := authorizeOperation(&Registry{}, actor, args)
	digest := authorityDigest(actor.ID, args)
	if receiptErr := appendAuthorityDecision(digest, actor.ID, operation, allowed, reason); receiptErr != nil {
		return fmt.Errorf("record authority decision: %w", receiptErr)
	}
	if !allowed {
		return fmt.Errorf("authenticated authority refused: %s", reason)
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

func authorizeOperation(reg *Registry, actor *Session, args []string) (bool, string) {
	if actor == nil || len(args) == 0 {
		return false, "missing actor or operation"
	}
	// These operations cross a genuine human/security boundary. In particular,
	// Relay never declares host availability or performs login/credential/trust
	// setup on behalf of an authenticated agent.
	if args[0] == "auth" || args[0] == "host" || args[0] == "policy" || args[0] == "install-cmux-restore" ||
		(args[0] == "root" && len(args) > 1 && args[1] == "control-plane") {
		return false, "external trust, credential, security, or human policy operation"
	}
	if args[0] == "root" {
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
	if args[0] == "parent" && len(args) >= 2 && (args[1] == "link" || args[1] == "move" || args[1] == "reparent") {
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
	if args[0] == "parent" {
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
	if args[0] == "resolve" && len(args) >= 2 {
		msg, err := (&ParentService{Reg: reg}).FindMessage(args[1])
		if err != nil || !(actor.ID == msg.ParentSessionID || sessionAncestor(reg, actor.ID, msg.ParentSessionID)) {
			return false, "message is outside caller lineage"
		}
		return true, "manager message authority"
	}
	if args[0] == "agent" && len(args) >= 2 && args[1] != "start" {
		if len(args) < 3 {
			return false, "agent operation requires a handoff"
		}
		ho, err := reg.GetHandoff(args[2])
		if err != nil || !(actor.ID == ho.SourceSessionID || sessionAncestor(reg, actor.ID, ho.SourceSessionID)) {
			return false, "handoff is outside caller lineage"
		}
		return true, "manager handoff authority"
	}
	if args[0] == "session" {
		if len(args) == 2 && args[1] == "list" {
			return true, "authenticated session discovery"
		}
		if len(args) == 3 && args[1] == "cleanup" {
			target, err := reg.GetSession(args[2])
			if err == nil && target.SourceSessionID == actor.ID {
				return true, "immediate-child cleanup"
			}
		}
		return false, "session operation is outside authenticated scope"
	}
	if args[0] == "resume" && len(args) == 3 && args[1] == "list" && args[2] == "--probe" {
		return true, "authenticated session discovery"
	}
	// Existing agent, handoff, messaging, discovery, board and diagnostic verbs
	// operate on identities resolved by their services. Their bridge authority
	// derives from authenticated scope; destructive/external verbs above remain
	// explicitly excluded.
	allowed := map[string]bool{
		"agent": true, "handoff": true, "log": true,
		"msg": true, "board": true, "history": true,
		"targets": true, "doctor": true, "events": true, "help": true, "version": true,
	}
	if allowed[args[0]] {
		return true, "authenticated session scope"
	}
	return false, "operation is outside authenticated Relay scope"
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

func authorityDigest(actor string, args []string) string {
	sum := sha256.Sum256([]byte(actor + "\x00" + strings.Join(args, "\x00")))
	return hex.EncodeToString(sum[:])
}

// appendAuthorityDecision makes repeated bridge delivery idempotent. The
// authority lock covers both the duplicate scan and append, so concurrent
// retries yield exactly one durable audit record.
func appendAuthorityDecision(digest, actor, operation string, allowed bool, reason string) error {
	if err := EnsureAuthorityWritable(); err != nil {
		return err
	}
	unlock, err := lockAuthorityWrite()
	if err != nil {
		return err
	}
	defer unlock()
	if file, openErr := os.Open(LedgerPath()); openErr == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var record struct {
				Type   string `json:"type"`
				Digest string `json:"request_digest"`
			}
			if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Type == "authority_decision" && record.Digest == digest {
				_ = file.Close()
				return nil
			}
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return err
		}
		_ = file.Close()
	} else if !os.IsNotExist(openErr) {
		return openErr
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	return appendLedgerLocked(map[string]any{
		"v": 1, "type": "authority_decision", "ts": time.Now().UTC().Format(time.RFC3339Nano),
		"receipt_id": "ar-" + digest[:16], "request_digest": digest,
		"actor_session_id": actor, "operation": operation, "decision": decision, "reason": reason,
	})
}
