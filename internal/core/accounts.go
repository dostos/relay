package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Which login an agent CLI runs as, and whether it can, is agent-accounts'
// fact (workspace decision 2026-09-13, one owner per axis). relay used to
// re-derive it — parsing `ccs auth list` tables and `codex-multi-auth list`
// JSON over ssh — and that copy had already drifted from the catalog's. It
// now asks the one implementation through its JSON CLI:
//
//	agent-accounts [--host H] --json logins|status|…
//
// `--host` makes the CLI do the ssh itself with the workspace's fleet config,
// so relay and everything else reach the same box for the same alias. The
// binary is resolved from $RELAY_AGENT_ACCOUNTS, then PATH; when it is
// absent relay says so instead of guessing, because a guess here is the
// duplicate this file exists to remove.

// AccountsBinEnv overrides where the agent-accounts CLI is found.
const AccountsBinEnv = "RELAY_AGENT_ACCOUNTS"

// ErrAccountsCLIMissing is returned when no agent-accounts binary resolves.
var ErrAccountsCLIMissing = errors.New("agent-accounts is not installed on this Mac (pip install -e projects/infrastructure/agent-accounts)")

// AccountsRunner runs one agent-accounts invocation. args are everything
// after the binary (relay always passes --json first). Tests inject one; the
// default execs the resolved binary.
type AccountsRunner func(ctx context.Context, args []string) (stdout string, err error)

// DefaultAccountsRunner resolves the CLI lazily, once per call, so a binary
// installed after relay started is found without a restart.
func DefaultAccountsRunner() AccountsRunner {
	return func(ctx context.Context, args []string) (string, error) {
		bin, err := resolveAccountsBin()
		if err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			detail := strings.TrimSpace(errOut.String())
			if detail == "" {
				detail = err.Error()
			}
			return out.String(), fmt.Errorf("agent-accounts %s: %s", strings.Join(args, " "), detail)
		}
		return out.String(), nil
	}
}

func resolveAccountsBin() (string, error) {
	if v := strings.TrimSpace(os.Getenv(AccountsBinEnv)); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("%s=%s: %w", AccountsBinEnv, v, err)
		}
		return v, nil
	}
	bin, err := exec.LookPath("agent-accounts")
	if err != nil {
		return "", ErrAccountsCLIMissing
	}
	return bin, nil
}

// AccountLogin is one row of `agent-accounts --json logins`.
type AccountLogin struct {
	Backend   string   `json:"backend"`
	Name      string   `json:"name"`
	Handle    string   `json:"handle"`
	Enabled   bool     `json:"enabled"`
	IsDefault bool     `json:"is_default"`
	Markers   []string `json:"markers"`
	Usable    bool     `json:"usable"`
}

// AccountStatus is one row of `agent-accounts --json status`. Confidence is
// the load-bearing field: `local-expiry` says NOT EXPIRED and cannot say
// USABLE (a token two hours from expiry was refused because its org was
// deactivated); `declared` is the manager's own claim; `none` means one host
// login and no manager to ask.
type AccountStatus struct {
	Backend    string `json:"backend"`
	Name       string `json:"name"`
	Handle     string `json:"handle"`
	State      string `json:"state"`
	Confidence string `json:"confidence"`
	Detail     string `json:"detail"`
	ExpiresInS *int   `json:"expires_in_s"`
	Caveat     string `json:"caveat,omitempty"`
}

// accountsHostArgs turns a relay host id into the CLI's --host. The local
// identity is this machine, which the CLI already is.
func accountsHostArgs(hostID string) []string {
	if hostID == "" || hostID == LocalHostID || hostID == "self" {
		return nil
	}
	if local := LocalHostIDFromProfile(); local != "" && hostID == local {
		return nil
	}
	return []string{"--host", hostID}
}

// AccountLogins lists a host's logins, optionally for one backend.
func AccountLogins(ctx context.Context, run AccountsRunner, hostID, backend string) ([]AccountLogin, error) {
	if run == nil {
		return nil, ErrAccountsCLIMissing
	}
	args := append([]string{"--json"}, accountsHostArgs(hostID)...)
	args = append(args, "logins")
	if backend != "" {
		args = append(args, "--backend", backend)
	}
	out, err := run(ctx, args)
	if err != nil {
		return nil, err
	}
	var rows []AccountLogin
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		return nil, fmt.Errorf("agent-accounts logins: unreadable output: %w", err)
	}
	return rows, nil
}

// AccountStatuses reports every login's auth health on a host.
func AccountStatuses(ctx context.Context, run AccountsRunner, hostID string) ([]AccountStatus, error) {
	if run == nil {
		return nil, ErrAccountsCLIMissing
	}
	args := append([]string{"--json"}, accountsHostArgs(hostID)...)
	args = append(args, "status")
	out, err := run(ctx, args)
	if err != nil {
		return nil, err
	}
	var rows []AccountStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		return nil, fmt.Errorf("agent-accounts status: unreadable output: %w", err)
	}
	return rows, nil
}

var accountEmailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// AgentNameForLogin is relay's NAME for a login — the spelling host.yaml
// agents and Forge use. The facts behind the name come from the catalog; only
// the spelling is relay's:
//
//   - a claude login is `ccs:<profile>`;
//   - a codex login is `codex:<selector>`, where the selector is what
//     `codex-multi-auth-codex --account` accepts: the account's email when its
//     label carries one, else its 1-based index (the catalog's handle is the
//     0-based index `switch` takes);
//   - anything else is the backend itself: one host login, nothing to select.
func AgentNameForLogin(l AccountLogin) string {
	switch l.Backend {
	case "claude":
		return "ccs:" + l.Name
	case "codex":
		return "codex:" + codexSelector(l.Name, l.Handle)
	default:
		return l.Backend
	}
}

func codexSelector(name, handle string) string {
	if email := accountEmailRe.FindString(name); email != "" {
		return email
	}
	if idx, err := strconv.Atoi(strings.TrimSpace(handle)); err == nil && idx >= 0 {
		return strconv.Itoa(idx + 1)
	}
	return strings.TrimSpace(handle)
}

// agentSpecForLogin is the host.yaml entry relay proposes for a login.
func agentSpecForLogin(l AccountLogin) AgentSpec {
	name := AgentNameForLogin(l)
	switch l.Backend {
	case "claude":
		// The CLI a ccs profile runs is claude; activation is the profile's
		// config home (the catalog's `process_env`), never `ccs <profile> …`
		// as a wrapper — ccs writes sync chatter to stdout (`never_wrap`).
		return AgentSpec{Name: name, Command: "claude"}
	case "codex":
		return AgentSpec{
			Name:     name,
			Command:  "codex-multi-auth-codex",
			Args:     []string{"--account", strings.TrimPrefix(name, "codex:")},
			UsageKey: "codex",
		}
	default:
		return AgentSpec{Name: name, Command: l.Backend}
	}
}

// statusRowForLogin renders one catalog status as relay's auth row.
func statusRowForLogin(st AccountStatus) AuthStatusRow {
	login := AccountLogin{Backend: st.Backend, Name: st.Name, Handle: st.Handle}
	spec := agentSpecForLogin(login)
	detail := st.Detail
	if st.Confidence != "" {
		detail = st.Confidence + ": " + detail
	}
	return AuthStatusRow{
		Agent:      spec.Name,
		Backend:    st.Backend,
		Handle:     st.Handle,
		Present:    true,
		Authed:     st.State == "ok",
		State:      st.State,
		Confidence: st.Confidence,
		Detail:     truncate(detail, 240),
		Login:      LoginCommand(spec),
		CopyOK:     len(CredentialPaths(spec)) > 0,
	}
}
