package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
)

// OfferApplyEnvelope is the immutable JSON payload stored in `<vault>/approvals/payloads/<id>.json`
// and piped via `/dev/fd/3` into `hq connector apply-offer` at `hq approve` time.
type OfferApplyEnvelope struct {
	SchemaVersion      int               `json:"schema_version"`
	Connector          string            `json:"connector"`
	Account            string            `json:"account"`
	Action             string            `json:"action"`
	Verb               string            `json:"verb"`
	DeliveryMode       string            `json:"delivery_mode,omitempty"`
	ThreadKey          string            `json:"thread_key,omitempty"`
	ExpectedHeadItemID string            `json:"expected_head_item_id,omitempty"`
	Params             map[string]string `json:"params,omitempty"`
	Body               string            `json:"body"`
}

// ParseRFC5322Emails parses an RFC 5322 address list (`To`, `Cc`, `From`) and returns lowercase `addr.Address` values ONLY,
// discarding display names (`"ceo@acme.com" <x@evil.com>` -> `["x@evil.com"]`).
func ParseRFC5322Emails(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(trimmed)
	if err != nil {
		single, sErr := mail.ParseAddress(trimmed)
		if sErr != nil {
			return nil, fmt.Errorf("invalid RFC 5322 address %q: %w", raw, err)
		}
		list = []*mail.Address{single}
	}
	var out []string
	seen := make(map[string]bool, len(list))
	for _, a := range list {
		addr := strings.ToLower(strings.TrimSpace(a.Address))
		if addr == "" || !strings.Contains(addr, "@") {
			return nil, fmt.Errorf("malformed email address %q", a.Address)
		}
		if !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out, nil
}

// IsDKIMOrSPFAligned verifies whether Authentication-Results proves DKIM or SPF alignment for fromEmail.
func IsDKIMOrSPFAligned(fromEmail, authResults string, isMock bool) bool {
	fromEmail = strings.ToLower(strings.TrimSpace(fromEmail))
	_, fromDomain, hasAt := strings.Cut(fromEmail, "@")
	if !hasAt || fromDomain == "" {
		return false
	}
	ar := strings.ToLower(strings.TrimSpace(authResults))
	if ar == "" {
		return isMock
	}
	dkimPass := strings.Contains(ar, "dkim=pass")
	spfPass := strings.Contains(ar, "spf=pass")
	if !dkimPass && !spfPass {
		return false
	}
	// If header.i=@domain or header.d=domain is present, check domain alignment
	if strings.Contains(ar, "header.d=") || strings.Contains(ar, "header.i=") || strings.Contains(ar, "smtp.mailfrom=") {
		return strings.Contains(ar, fromDomain)
	}
	return true
}

func classifyErrCode(err error) string {
	if err == nil {
		return ""
	}
	if pe, ok := err.(*ProtocolError); ok && pe.Code != "" {
		return pe.Code
	}
	lower := strings.ToLower(err.Error())
	for _, kw := range []string{"401", "403", "unauthorized", "invalid_grant", "token expired", "sign in", "re-authenticate", "no active desktop conversation", "credential_scope"} {
		if strings.Contains(lower, kw) {
			return "auth_expired"
		}
	}
	for _, kw := range []string{"not found", "invalid connector", "strict", "untrusted", "sha-256 mismatch"} {
		if strings.Contains(lower, kw) {
			return "permanent"
		}
	}
	return "transient"
}

func resolveAccountInstance(rc ResolvedConnector, alias string) (string, AccountInstance, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		alias = strings.TrimSpace(rc.Instance.DefaultAccount)
	}
	if alias != "" {
		if acc, ok := rc.Instance.Accounts[alias]; ok {
			return alias, acc, nil
		}
	}
	if len(rc.Instance.Accounts) == 1 {
		for k, v := range rc.Instance.Accounts {
			return k, v, nil
		}
	}
	if len(rc.Instance.Accounts) > 0 && alias == "" {
		keys := make([]string, 0, len(rc.Instance.Accounts))
		for k := range rc.Instance.Accounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys[0], rc.Instance.Accounts[keys[0]], nil
	}
	if alias == "" {
		alias = "default"
	}
	return alias, AccountInstance{}, nil
}

func resolveVerifiedCLIBinary(vaultDir string, rc ResolvedConnector) (string, error) {
	explicit := ""
	if rc.Manifest.Transport.CLI != nil {
		explicit = strings.TrimSpace(rc.Manifest.Transport.CLI.Path)
	}
	// Honor legacy gmail accounts.json cliPath only when connector manifest leaves cli.path blank
	if explicit == "" && rc.ID == "gmail" {
		explicit = lookupLegacyGmailCLIPath(vaultDir)
	}
	if explicit != "" {
		if rc.SourceID != "builtin" {
			return extension.VerifyTrustedBinary(vaultDir, rc.SourceID, explicit)
		}
		if extension.IsSelfHQBinary(explicit) {
			return extension.ResolveSelfHQExecutable(), nil
		}
		if filepath.IsAbs(explicit) {
			if st, err := os.Stat(explicit); err == nil && !st.IsDir() {
				return explicit, nil
			}
		}
		if lp, err := exec.LookPath(explicit); err == nil && lp != "" {
			return lp, nil
		}
		return "", fmt.Errorf("configured CLI binary %q for connector %s not found", explicit, rc.ID)
	}

	// Built-in host binary discovery via hostpaths (never auto-downloading unpinned plugins!)
	binName := rc.ID
	if lp, err := exec.LookPath(binName); err == nil && lp != "" {
		return lp, nil
	}
	roots := hostpaths.Resolve(vaultDir)
	if roots.AppDataDir != "" {
		matches, _ := filepath.Glob(filepath.Join(roots.AppDataDir, "plugin_data", binName, "bin", binName+"-*"))
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	if roots.PluginsDir != "" {
		launcher := filepath.Join(roots.PluginsDir, binName, "skills", binName, "scripts", binName)
		if st, err := os.Stat(launcher); err == nil && !st.IsDir() {
			return launcher, nil
		}
	}
	sysMatches, _ := filepath.Glob("/usr/share/antigravity/resources/app/extensions/antigravity/bin/" + binName + "-*")
	if len(sysMatches) > 0 {
		return sysMatches[0], nil
	}
	return "", &ProtocolError{
		Code:    "permanent",
		Message: fmt.Sprintf("CLI binary for connector %q not found on PATH or plugin dirs (configure cli.path or install plugin explicitly)", rc.ID),
	}
}

// ExecCLIVerb executes a named `transport.cli.verbs[verb]` command through the scrubbed subprocess pool,
// optionally streaming `payloadBytes` via `/dev/fd/3` (`{{payload_file}}`).
func ExecCLIVerb(ctx context.Context, vaultDir, connID, account, verb string, scalarParams map[string]string, listParams map[string][]string, payloadBytes []byte, allowDesktopScope bool) ([]byte, error) {
	rc, err := Resolve(vaultDir, connID)
	if err != nil {
		return nil, err
	}
	return execResolvedCLIVerb(ctx, vaultDir, rc, account, verb, scalarParams, listParams, payloadBytes, allowDesktopScope)
}

func execResolvedCLIVerb(ctx context.Context, vaultDir string, rc ResolvedConnector, account, verb string, scalarParams map[string]string, listParams map[string][]string, payloadBytes []byte, allowDesktopScope bool) ([]byte, error) {
	if rc.Manifest.RequiresDesktopSession && !allowDesktopScope {
		return nil, &ProtocolError{
			Code:    "auth_expired",
			Message: "paused (no active desktop conversation)",
		}
	}
	if rc.Manifest.Transport.CLI == nil {
		return nil, &ProtocolError{
			Code:    "permanent",
			Message: fmt.Sprintf("connector %q does not configure transport.cli", rc.ID),
		}
	}
	tmpl, ok := rc.Manifest.Transport.CLI.Verbs[verb]
	if !ok || len(tmpl) == 0 {
		return nil, &ProtocolError{
			Code:    "permanent",
			Message: fmt.Sprintf("connector %q does not declare CLI verb %q", rc.ID, verb),
		}
	}
	binPath, err := resolveVerifiedCLIBinary(vaultDir, rc)
	if err != nil {
		return nil, err
	}
	payloadArg := ""
	if payloadBytes != nil {
		payloadArg = "/dev/fd/3"
	}
	argv, err := ExpandVerbArgv(tmpl, scalarParams, listParams, payloadArg)
	if err != nil {
		return nil, &ProtocolError{Code: "permanent", Message: err.Error()}
	}
	_, accCfg, _ := resolveAccountInstance(rc, account)
	extraEnv := make(map[string]string)
	if strings.TrimSpace(accCfg.AgyAccount) != "" {
		extraEnv["AGY_ACCOUNT"] = strings.TrimSpace(accCfg.AgyAccount)
	}
	stdout, _, runErr := RunSubprocess(ctx, SubprocessSpec{
		Binary:            binPath,
		Args:              argv,
		EnvAllow:          rc.Manifest.EnvAllow,
		ExtraEnv:          extraEnv,
		AllowDesktopScope: allowDesktopScope,
		PayloadBytes:      payloadBytes,
		Timeout:           DefaultSubprocessTimeout,
	})
	if runErr != nil {
		return nil, &ProtocolError{Code: classifyErrCode(runErr), Message: runErr.Error()}
	}
	return stdout, nil
}

// Execute dispatches a `ProtocolRequest` to either an external `hq-connector-protocol/v1` command
// or one of the built-in drivers (`jsonmap`, `gmail`, `gchat`, `gcalendar`).
func Execute(ctx context.Context, vaultDir string, req ProtocolRequest, allowDesktopScope bool) (ProtocolResponse, error) {
	req.ProtocolVersion = ProtocolVersionV1
	rc, err := Resolve(vaultDir, req.Connector)
	if err != nil {
		return ProtocolResponse{}, err
	}
	if !rc.Instance.IsEnabled() {
		return ProtocolResponse{ProtocolVersion: ProtocolVersionV1, OK: true}, nil
	}
	alias, accCfg, err := resolveAccountInstance(rc, req.Account)
	if err != nil {
		return ProtocolResponse{}, err
	}
	req.Account = alias
	if !accCfg.IsEnabled() {
		return ProtocolResponse{ProtocolVersion: ProtocolVersionV1, OK: true}, nil
	}
	if rc.Manifest.RequiresDesktopSession && !allowDesktopScope && !hasConnectorMock(vaultDir, rc.ID) {
		return ProtocolResponse{}, &ProtocolError{
			Code:    "auth_expired",
			Message: "paused (no active desktop conversation)",
		}
	}

	if len(rc.Manifest.Driver.Command) > 0 {
		return execExternalProtocolDriver(ctx, vaultDir, rc, accCfg, req, allowDesktopScope)
	}

	switch strings.ToLower(strings.TrimSpace(rc.Manifest.Driver.Builtin)) {
	case "jsonmap":
		return evalJSONMapConnector(ctx, vaultDir, rc, req, allowDesktopScope)
	case "gmail":
		return evalBuiltinGmail(ctx, vaultDir, rc, alias, accCfg, req, allowDesktopScope)
	case "gchat":
		return evalBuiltinGChat(ctx, vaultDir, rc, alias, accCfg, req, allowDesktopScope)
	case "gcalendar":
		return evalBuiltinGCalendar(ctx, vaultDir, rc, alias, accCfg, req, allowDesktopScope)
	default:
		return ProtocolResponse{}, &ProtocolError{
			Code:    "permanent",
			Message: fmt.Sprintf("unknown builtin driver %q on connector %q", rc.Manifest.Driver.Builtin, rc.ID),
		}
	}
}

func execExternalProtocolDriver(ctx context.Context, vaultDir string, rc ResolvedConnector, accCfg AccountInstance, req ProtocolRequest, allowDesktopScope bool) (ProtocolResponse, error) {
	cmd0 := rc.Manifest.Driver.Command[0]
	binPath, err := extension.VerifyTrustedBinary(vaultDir, rc.SourceID, cmd0)
	if err != nil {
		return ProtocolResponse{}, &ProtocolError{Code: "permanent", Message: err.Error()}
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return ProtocolResponse{}, err
	}
	extraEnv := map[string]string{}
	if strings.TrimSpace(accCfg.AgyAccount) != "" {
		extraEnv["AGY_ACCOUNT"] = strings.TrimSpace(accCfg.AgyAccount)
	}
	stdout, _, runErr := RunSubprocess(ctx, SubprocessSpec{
		Binary:            binPath,
		Args:              rc.Manifest.Driver.Command[1:],
		EnvAllow:          rc.Manifest.EnvAllow,
		ExtraEnv:          extraEnv,
		AllowDesktopScope: allowDesktopScope,
		StdinBytes:        append(reqBytes, '\n'),
		Timeout:           DefaultSubprocessTimeout,
	})
	if runErr != nil && len(bytes.TrimSpace(stdout)) == 0 {
		return ProtocolResponse{}, &ProtocolError{Code: classifyErrCode(runErr), Message: runErr.Error()}
	}
	var resp ProtocolResponse
	if jErr := json.Unmarshal(stdout, &resp); jErr != nil {
		if runErr != nil {
			return ProtocolResponse{}, &ProtocolError{Code: classifyErrCode(runErr), Message: runErr.Error()}
		}
		return ProtocolResponse{}, &ProtocolError{Code: "permanent", Message: "invalid protocol/v1 stdout JSON: " + jErr.Error()}
	}
	if !resp.OK {
		if resp.Error != nil {
			return resp, resp.Error
		}
		return resp, &ProtocolError{Code: "transient", Message: "driver returned ok=false"}
	}
	return resp, nil
}

func evalJSONMapConnector(ctx context.Context, vaultDir string, rc ResolvedConnector, req ProtocolRequest, allowDesktopScope bool) (ProtocolResponse, error) {
	verb := req.Op
	if req.Op == "query" && req.Params != nil && req.Params["query_name"] != "" {
		if qSpec, ok := rc.Manifest.Queries[req.Params["query_name"]]; ok && qSpec.Verb != "" {
			verb = qSpec.Verb
		}
	}
	if rc.Manifest.Transport.CLI != nil {
		if _, hasVerb := rc.Manifest.Transport.CLI.Verbs[verb]; !hasVerb {
			if req.Op == "poll" {
				for _, fallback := range []string{"poll", "list", "search", "list_messages"} {
					if _, ok := rc.Manifest.Transport.CLI.Verbs[fallback]; ok {
						verb = fallback
						break
					}
				}
			} else if req.Op == "brief" {
				for _, fallback := range []string{"brief", "poll", "list", "search", "list_messages", "events"} {
					if _, ok := rc.Manifest.Transport.CLI.Verbs[fallback]; ok {
						verb = fallback
						break
					}
				}
			} else if req.Op == "read_quarantined" || req.Op == "inspect_thread" {
				for _, fallback := range []string{"get_thread", "get", "read"} {
					if _, ok := rc.Manifest.Transport.CLI.Verbs[fallback]; ok {
						verb = fallback
						break
					}
				}
			}
		}
	}

	params := make(map[string]string)
	for k, v := range req.Params {
		params[k] = v
	}
	if req.Cursor != "" {
		params["cursor"] = req.Cursor
	}
	if req.PageToken != "" {
		params["page_token"] = req.PageToken
	}
	if req.MaxItems > 0 {
		params["max"] = strconv.Itoa(req.MaxItems)
	}

	stdout, err := execResolvedCLIVerb(ctx, vaultDir, rc, req.Account, verb, params, nil, nil, allowDesktopScope)
	if err != nil {
		return ProtocolResponse{}, err
	}

	if req.Op == "query" {
		var qData map[string]interface{}
		if json.Unmarshal(stdout, &qData) != nil {
			var arr []interface{}
			if json.Unmarshal(stdout, &arr) == nil {
				qData = map[string]interface{}{"items": arr}
			}
		}
		return ProtocolResponse{ProtocolVersion: ProtocolVersionV1, OK: true, QueryData: qData}, nil
	}

	var jm JSONMapSpec
	if rc.Manifest.Transport.CLI != nil {
		if vo, ok := rc.Manifest.Transport.CLI.Output[verb]; ok && vo.JSONMap != nil {
			jm = *vo.JSONMap
			if jm.Format == "" {
				jm.Format = vo.Format
			}
		}
	}
	if jm.ItemID == "" {
		jm.ItemID = "/id"
	}
	items, nextCur, nextPage, mErr := MapCLIOutput(stdout, jm)
	if mErr != nil {
		return ProtocolResponse{}, &ProtocolError{Code: "permanent", Message: mErr.Error()}
	}
	resp := ProtocolResponse{
		ProtocolVersion: ProtocolVersionV1,
		OK:              true,
		NextCursor:      nextCur,
		NextPageToken:   nextPage,
		Items:           items,
	}
	if req.Op == "read_quarantined" {
		tKey := params["thread_key"]
		if tKey == "" && len(items) > 0 {
			tKey = items[0].ThreadKey
		}
		qt := &QuarantinedThread{
			Connector: rc.ID,
			Account:   req.Account,
			ThreadKey: tKey,
		}
		for _, it := range items {
			qt.HeadItemID = it.ItemID
			if qt.SubjectOrRoom == "" {
				qt.SubjectOrRoom = it.SubjectOrRoom
			}
			qt.Messages = append(qt.Messages, QuarantinedMessage{
				ItemID:    it.ItemID,
				Sender:    it.Sender,
				Timestamp: it.Timestamp,
				Body:      it.Body,
			})
		}
		resp.Thread = qt
	} else if req.Op == "inspect_thread" {
		_, accCfg, _ := resolveAccountInstance(rc, req.Account)
		ownerEmail := strings.TrimSpace(accCfg.Email)
		head := ""
		if len(items) > 0 {
			head = items[len(items)-1].ItemID
		}
		afterID := params["after_item_id"]
		var newer []NormalizedItem
		seenAfter := afterID == ""
		for _, it := range items {
			if seenAfter {
				newer = append(newer, it)
			} else if it.ItemID == afterID {
				seenAfter = true
			}
		}
		if !seenAfter && afterID != "" && len(items) > 0 && head != afterID {
			// CLI verb already filtered to newer items or returned latest head snapshot without afterID.
			newer = items
		}
		cpReplied := false
		latestCP := ""
		for _, ni := range newer {
			if ownerEmail == "" || !strings.EqualFold(strings.TrimSpace(ni.Sender), ownerEmail) {
				cpReplied = true
				latestCP = ni.Sender
			}
		}
		resp.Inspection = &ThreadInspection{
			ThreadKey:                params["thread_key"],
			HeadItemID:               head,
			NewerItems:               newer,
			CounterpartyReplied:      cpReplied,
			LatestCounterpartySender: latestCP,
		}
	}
	return resp, nil
}

// --- Built-in Gmail Driver (Mock + Direct OAuth w/ Refresh Expiry + CLI Transport) ---

type rawMIMEPart struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Body     struct {
		AttachmentID string `json:"attachmentId"`
		Size         int64  `json:"size"`
		Data         string `json:"data"`
	} `json:"body"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Parts []rawMIMEPart `json:"parts"`
}

type rawMailMsg struct {
	ID           string      `json:"id"`
	ThreadID     string      `json:"threadId"`
	Snippet      string      `json:"snippet"`
	InternalDate string      `json:"internalDate"`
	From         string      `json:"from"`
	To           string      `json:"to"`
	Cc           string      `json:"cc"`
	Subject      string      `json:"subject"`
	Date         string      `json:"date"`
	PlainBody    string      `json:"plainBody"`
	HTMLBody     string      `json:"htmlBody"`
	AuthResults  string      `json:"authResults"`
	LabelIDs     []string    `json:"labelIds"`
	Payload      rawMIMEPart `json:"payload"`
}

func decodeB64URL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return ""
}

func extractMIMEPlain(p rawMIMEPart) (string, string) {
	var plainOut, htmlOut string
	var walk func(part rawMIMEPart)
	walk = func(part rawMIMEPart) {
		mt := strings.ToLower(part.MimeType)
		if mt == "text/plain" && part.Body.Data != "" && plainOut == "" {
			plainOut = decodeB64URL(part.Body.Data)
		} else if mt == "text/html" && part.Body.Data != "" && htmlOut == "" {
			htmlOut = decodeB64URL(part.Body.Data)
		}
		for _, sub := range part.Parts {
			walk(sub)
		}
	}
	walk(p)
	return plainOut, htmlOut
}

func normalizeRawMail(rm rawMailMsg) (id, threadID, from, to, cc, subject, ts, body, authRes string) {
	id = strings.TrimSpace(rm.ID)
	threadID = strings.TrimSpace(rm.ThreadID)
	if threadID == "" {
		threadID = id
	}
	from = rm.From
	to = rm.To
	cc = rm.Cc
	subject = rm.Subject
	ts = rm.Date
	authRes = rm.AuthResults
	for _, h := range rm.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			if from == "" {
				from = h.Value
			}
		case "to":
			if to == "" {
				to = h.Value
			}
		case "cc":
			if cc == "" {
				cc = h.Value
			}
		case "subject":
			if subject == "" {
				subject = h.Value
			}
		case "date":
			if ts == "" {
				ts = h.Value
			}
		case "authentication-results":
			if authRes == "" {
				authRes = h.Value
			}
		}
	}
	plainB, htmlB := extractMIMEPlain(rm.Payload)
	if rm.PlainBody != "" {
		plainB = rm.PlainBody
	}
	if plainB != "" {
		body = plainB
	} else if rm.HTMLBody != "" {
		body = rm.HTMLBody
	} else if htmlB != "" {
		body = htmlB
	} else {
		body = html.UnescapeString(rm.Snippet)
	}
	if rm.InternalDate != "" {
		if ms, err := strconv.ParseInt(rm.InternalDate, 10, 64); err == nil && ms > 0 {
			ts = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
	}
	return
}

func hasConnectorMock(vaultDir, connID string) bool {
	switch connID {
	case "gmail":
		if os.Getenv("HQ_GMAIL_MOCK_DIR") != "" {
			return true
		}
		st, err := os.Stat(filepath.Join(vaultDir, "memory", "gmail", "mock"))
		return err == nil && st.IsDir()
	case "gchat":
		if os.Getenv("HQ_GCHAT_MOCK_DIR") != "" {
			return true
		}
		st, err := os.Stat(filepath.Join(vaultDir, "memory", "gchat", "mock"))
		return err == nil && st.IsDir()
	case "gcalendar":
		_, err := os.Stat(filepath.Join(vaultDir, "memory", "gcalendar", "mock.json"))
		return err == nil
	}
	return false
}

// RefreshableOAuthToken loads an OAuth token JSON and refreshes `access_token` when `expiry` has passed (or within 60s).
func RefreshableOAuthToken(tokenPath string) (string, error) {
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		TokenURI     string `json:"token_uri"`
		Expiry       string `json:"expiry"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", err
	}
	expired := false
	if strings.TrimSpace(tok.Expiry) != "" {
		if expTime, pErr := time.Parse(time.RFC3339, strings.TrimSpace(tok.Expiry)); pErr == nil {
			if !time.Now().Add(60 * time.Second).Before(expTime) {
				expired = true
			}
		}
	}
	if tok.AccessToken != "" && !expired {
		return strings.TrimSpace(tok.AccessToken), nil
	}
	if tok.RefreshToken != "" && tok.ClientID != "" && tok.ClientSecret != "" {
		tokenURI := strings.TrimSpace(tok.TokenURI)
		if tokenURI == "" {
			tokenURI = "https://oauth2.googleapis.com/token"
		}
		form := url.Values{}
		form.Set("client_id", tok.ClientID)
		form.Set("client_secret", tok.ClientSecret)
		form.Set("refresh_token", tok.RefreshToken)
		form.Set("grant_type", "refresh_token")
		req, err := http.NewRequest(http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest {
			return "", &ProtocolError{Code: "auth_expired", Message: fmt.Sprintf("OAuth refresh_token rejected (HTTP %d)", resp.StatusCode)}
		}
		var refreshed struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if json.NewDecoder(resp.Body).Decode(&refreshed) == nil && refreshed.AccessToken != "" {
			return strings.TrimSpace(refreshed.AccessToken), nil
		}
	}
	if expired {
		return "", &ProtocolError{Code: "auth_expired", Message: "OAuth access_token expired and no valid refresh_token available"}
	}
	return "", &ProtocolError{Code: "auth_expired", Message: "no valid access_token in token file"}
}

func resolveTokenPath(vaultDir string, ref *string) string {
	if ref == nil || strings.TrimSpace(*ref) == "" {
		return ""
	}
	p := strings.TrimSpace(*ref)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(vaultDir, p)
}

func loadGmailRawMessages(ctx context.Context, vaultDir string, rc ResolvedConnector, alias string, accCfg AccountInstance, op, query, threadKey, pageToken string, maxItems int, allowDesktopScope bool) ([]rawMailMsg, string, bool, error) {
	// 1. Mock directory
	mockDir := strings.TrimSpace(os.Getenv("HQ_GMAIL_MOCK_DIR"))
	if mockDir == "" {
		cand := filepath.Join(vaultDir, "memory", "gmail", "mock")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			mockDir = cand
		}
	}
	if mockDir != "" {
		if op == "thread" {
			for _, tp := range []string{
				filepath.Join(mockDir, "thread-"+threadKey+".json"),
				filepath.Join(mockDir, "threads", threadKey+".json"),
			} {
				if b, err := os.ReadFile(tp); err == nil {
					var wrap struct {
						Messages []rawMailMsg `json:"messages"`
					}
					if json.Unmarshal(b, &wrap) == nil && len(wrap.Messages) > 0 {
						return wrap.Messages, "", true, nil
					}
				}
			}
		}
		mp := filepath.Join(mockDir, "messages.json")
		if b, err := os.ReadFile(mp); err == nil {
			var list []rawMailMsg
			if json.Unmarshal(b, &list) != nil {
				var wrap struct {
					Messages []rawMailMsg `json:"messages"`
				}
				_ = json.Unmarshal(b, &wrap)
				list = wrap.Messages
			}
			if op == "thread" {
				var filtered []rawMailMsg
				for _, m := range list {
					if m.ThreadID == threadKey || m.ID == threadKey {
						filtered = append(filtered, m)
					}
				}
				return filtered, "", true, nil
			}
			return list, "", true, nil
		}
		return nil, "", true, nil
	}

	// 2. Direct OAuth REST when read token ref is configured and transport != "cli"
	readTokPath := resolveTokenPath(vaultDir, accCfg.TokenRefs.Read)
	if readTokPath != "" && accCfg.TransportKind != "cli" {
		tok, err := RefreshableOAuthToken(readTokPath)
		if err != nil {
			return nil, "", false, err
		}
		apiBase := strings.TrimRight(strings.TrimSpace(os.Getenv("HQ_GMAIL_API_BASE")), "/")
		if apiBase == "" {
			apiBase = "https://gmail.googleapis.com"
		}
		client := &http.Client{Timeout: 6 * time.Second}
		if op == "thread" {
			u := fmt.Sprintf("%s/gmail/v1/users/me/threads/%s?format=full", apiBase, url.PathEscape(threadKey))
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, rErr := client.Do(req)
			if rErr != nil {
				return nil, "", false, rErr
			}
			defer resp.Body.Close()
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				return nil, "", false, &ProtocolError{Code: "auth_expired", Message: fmt.Sprintf("Gmail REST HTTP %d", resp.StatusCode)}
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, "", false, fmt.Errorf("Gmail thread REST HTTP %d", resp.StatusCode)
			}
			var th struct {
				Messages []rawMailMsg `json:"messages"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&th); err != nil {
				return nil, "", false, err
			}
			return th.Messages, "", false, nil
		}

		q := url.Values{}
		if query != "" {
			q.Set("q", query)
		}
		if maxItems > 0 {
			q.Set("maxResults", strconv.Itoa(maxItems))
		}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		u := fmt.Sprintf("%s/gmail/v1/users/me/messages?%s", apiBase, q.Encode())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, rErr := client.Do(req)
		if rErr != nil {
			return nil, "", false, rErr
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, "", false, &ProtocolError{Code: "auth_expired", Message: fmt.Sprintf("Gmail list REST HTTP %d", resp.StatusCode)}
		}
		var listResp struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			return nil, "", false, err
		}
		var msgs []rawMailMsg
		for _, stub := range listResp.Messages {
			mu := fmt.Sprintf("%s/gmail/v1/users/me/messages/%s?format=full", apiBase, url.PathEscape(stub.ID))
			mReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, mu, nil)
			mReq.Header.Set("Authorization", "Bearer "+tok)
			mResp, mErr := client.Do(mReq)
			if mErr != nil {
				continue
			}
			var full rawMailMsg
			_ = json.NewDecoder(mResp.Body).Decode(&full)
			_ = mResp.Body.Close()
			if full.ID != "" {
				msgs = append(msgs, full)
			}
		}
		return msgs, listResp.NextPageToken, false, nil
	}

	// 3. Unified CLI transport (`transport.cli`)
	verb := "search"
	params := map[string]string{}
	if op == "thread" {
		verb = "get_thread"
		params["thread_key"] = threadKey
	} else {
		if query == "" {
			query = "label:INBOX is:unread"
		}
		params["query"] = query
		if maxItems > 0 {
			params["max"] = strconv.Itoa(maxItems)
		}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
	}
	stdout, err := execResolvedCLIVerb(ctx, vaultDir, rc, alias, verb, params, nil, nil, allowDesktopScope)
	if err != nil {
		return nil, "", false, err
	}
	var arr []rawMailMsg
	if json.Unmarshal(stdout, &arr) == nil {
		return arr, "", false, nil
	}
	var wrap struct {
		Messages      []rawMailMsg `json:"messages"`
		NextPageToken string       `json:"nextPageToken"`
	}
	if err := json.Unmarshal(stdout, &wrap); err != nil {
		return nil, "", false, fmt.Errorf("decode Gmail CLI JSON: %w", err)
	}
	return wrap.Messages, wrap.NextPageToken, false, nil
}

func computeAuthenticatedEmailParticipants(ownerEmail string, msgs []rawMailMsg, isMock bool) []string {
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	authSet := make(map[string]bool)
	if ownerEmail != "" {
		authSet[ownerEmail] = true
	}
	for _, rm := range msgs {
		_, _, from, to, cc, _, _, _, authRes := normalizeRawMail(rm)
		fromAddrs, fErr := ParseRFC5322Emails(from)
		if fErr != nil || len(fromAddrs) == 0 {
			continue
		}
		senderAddr := fromAddrs[0]
		if ownerEmail != "" && strings.EqualFold(senderAddr, ownerEmail) {
			// Owner-sent message: recipients in To and Cc are authenticated by owner action
			if toList, err := ParseRFC5322Emails(to); err == nil {
				for _, a := range toList {
					authSet[a] = true
				}
			}
			if ccList, err := ParseRFC5322Emails(cc); err == nil {
				for _, a := range ccList {
					authSet[a] = true
				}
			}
		} else if IsDKIMOrSPFAligned(senderAddr, authRes, isMock) {
			authSet[senderAddr] = true
		}
	}
	out := make([]string, 0, len(authSet))
	for a := range authSet {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func evalBuiltinGmail(ctx context.Context, vaultDir string, rc ResolvedConnector, alias string, accCfg AccountInstance, req ProtocolRequest, allowDesktopScope bool) (ProtocolResponse, error) {
	ownerEmail := strings.ToLower(strings.TrimSpace(accCfg.Email))
	switch req.Op {
	case "poll", "brief":
		q := accCfg.DefaultQuery
		if req.Params != nil && strings.TrimSpace(req.Params["query"]) != "" {
			q = strings.TrimSpace(req.Params["query"])
		}
		if q == "" {
			q = "label:INBOX is:unread category:primary -label:promotions -label:social"
		}
		maxItems := req.MaxItems
		if maxItems <= 0 {
			maxItems = 50
		}
		rawMsgs, nextPage, isMock, err := loadGmailRawMessages(ctx, vaultDir, rc, alias, accCfg, "list", q, "", req.PageToken, maxItems, allowDesktopScope)
		if err != nil {
			return ProtocolResponse{}, err
		}
		var items []NormalizedItem
		nextCur := req.Cursor
		for _, rm := range rawMsgs {
			id, thID, from, to, _, subj, ts, body, authRes := normalizeRawMail(rm)
			if id == "" {
				continue
			}
			senderAddr := from
			if parsed, pErr := ParseRFC5322Emails(from); pErr == nil && len(parsed) > 0 {
				senderAddr = parsed[0]
			}
			var authParts []string
			if IsDKIMOrSPFAligned(senderAddr, authRes, isMock) {
				authParts = append(authParts, senderAddr)
			}
			if ownerEmail != "" {
				authParts = append(authParts, ownerEmail)
			}
			lowerSubj := strings.ToLower(subj)
			lowerSender := strings.ToLower(senderAddr)
			isAuto := strings.Contains(lowerSender, "noreply") || strings.Contains(lowerSender, "no-reply") || strings.Contains(lowerSender, "notifications@")
			hasSec := strings.Contains(lowerSubj, "security alert") || strings.Contains(lowerSubj, "verification code")
			hasSched := strings.Contains(lowerSubj, "invitation:") || strings.Contains(lowerSubj, "reschedule") || strings.Contains(strings.ToLower(body), "calendar")
			isDirect := ownerEmail == "" || strings.Contains(strings.ToLower(to), ownerEmail)
			items = append(items, NormalizedItem{
				ItemID:                    id,
				ThreadKey:                 thID,
				Timestamp:                 ts,
				Sender:                    senderAddr,
				AuthenticatedParticipants: authParts,
				SubjectOrRoom:             subj,
				Body:                      body,
				IsDirect:                  isDirect,
				IsAutomated:               isAuto,
				HasSecurityOverride:       hasSec,
				HasSchedule:               hasSched,
			})
			nextCur = id
		}
		return ProtocolResponse{
			ProtocolVersion: ProtocolVersionV1,
			OK:              true,
			NextCursor:      nextCur,
			NextPageToken:   nextPage,
			Items:           items,
		}, nil

	case "inspect_thread", "read_quarantined":
		tKey := ""
		afterID := ""
		if req.Params != nil {
			tKey = strings.TrimSpace(req.Params["thread_key"])
			afterID = strings.TrimSpace(req.Params["after_item_id"])
		}
		if tKey == "" {
			return ProtocolResponse{}, &ProtocolError{Code: "permanent", Message: "thread_key is required"}
		}
		rawMsgs, _, isMock, err := loadGmailRawMessages(ctx, vaultDir, rc, alias, accCfg, "thread", "", tKey, "", 50, allowDesktopScope)
		if err != nil {
			return ProtocolResponse{}, err
		}
		authParts := computeAuthenticatedEmailParticipants(ownerEmail, rawMsgs, isMock)
		qt := &QuarantinedThread{
			Connector:                 rc.ID,
			Account:                   alias,
			ThreadKey:                 tKey,
			AuthenticatedParticipants: authParts,
		}
		var newer []NormalizedItem
		seenAfter := afterID == ""
		counterpartyReplied := false
		latestCounterparty := ""

		for _, rm := range rawMsgs {
			id, _, from, to, cc, subj, ts, body, authRes := normalizeRawMail(rm)
			if id == "" {
				continue
			}
			senderAddr := from
			if parsed, pErr := ParseRFC5322Emails(from); pErr == nil && len(parsed) > 0 {
				senderAddr = parsed[0]
			}
			qt.HeadItemID = id
			if qt.SubjectOrRoom == "" && subj != "" {
				qt.SubjectOrRoom = subj
			}
			aligned := IsDKIMOrSPFAligned(senderAddr, authRes, isMock)
			qt.Messages = append(qt.Messages, QuarantinedMessage{
				ItemID:           id,
				Sender:           senderAddr,
				To:               to,
				Cc:               cc,
				Timestamp:        ts,
				Body:             body,
				DKIMOrSPFAligned: aligned,
			})
			if seenAfter {
				newer = append(newer, NormalizedItem{
					ItemID:        id,
					ThreadKey:     tKey,
					Timestamp:     ts,
					Sender:        senderAddr,
					SubjectOrRoom: subj,
				})
				if ownerEmail == "" || !strings.EqualFold(senderAddr, ownerEmail) {
					counterpartyReplied = true
					latestCounterparty = senderAddr
				}
			} else if id == afterID {
				seenAfter = true
			}
		}
		return ProtocolResponse{
			ProtocolVersion: ProtocolVersionV1,
			OK:              true,
			Thread:          qt,
			Inspection: &ThreadInspection{
				ThreadKey:                 tKey,
				HeadItemID:                qt.HeadItemID,
				NewerItems:                newer,
				AuthenticatedParticipants: authParts,
				CounterpartyReplied:       counterpartyReplied,
				LatestCounterpartySender:  latestCounterparty,
			},
		}, nil
	default:
		return ProtocolResponse{}, &ProtocolError{Code: "permanent", Message: "unsupported Gmail op: " + req.Op}
	}
}

// --- Built-in GChat Driver (Mock + CLI + OAuth REST) ---

func evalBuiltinGChat(ctx context.Context, vaultDir string, rc ResolvedConnector, alias string, accCfg AccountInstance, req ProtocolRequest, allowDesktopScope bool) (ProtocolResponse, error) {
	ownerEmail := strings.ToLower(strings.TrimSpace(accCfg.Email))
	mockDir := strings.TrimSpace(os.Getenv("HQ_GCHAT_MOCK_DIR"))
	if mockDir == "" {
		cand := filepath.Join(vaultDir, "memory", "gchat", "mock")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			mockDir = cand
		}
	}
	if mockDir != "" {
		mp := filepath.Join(mockDir, "messages.json")
		raw, _ := os.ReadFile(mp)
		var msgs []struct {
			ID         string `json:"id"`
			SpaceID    string `json:"spaceId"`
			SpaceName  string `json:"spaceName"`
			ThreadID   string `json:"threadId"`
			Sender     string `json:"sender"`
			CreateTime string `json:"createTime"`
			Snippet    string `json:"snippet"`
			PlainText  string `json:"plainText"`
		}
		_ = json.Unmarshal(raw, &msgs)
		wantSpace := ""
		afterID := ""
		if req.Params != nil {
			wantSpace = strings.TrimSpace(req.Params["thread_key"])
			if wantSpace == "" {
				wantSpace = strings.TrimSpace(req.Params["space"])
			}
			afterID = strings.TrimSpace(req.Params["after_item_id"])
		}
		var items []NormalizedItem
		qt := &QuarantinedThread{Connector: rc.ID, Account: alias, ThreadKey: wantSpace}
		var newer []NormalizedItem
		seenAfter := afterID == ""
		replied := false
		latestSender := ""
		partsMap := map[string]bool{}
		for _, m := range msgs {
			thKey := m.ThreadID
			if thKey == "" {
				thKey = m.SpaceID
			}
			if wantSpace != "" && m.SpaceID != wantSpace && thKey != wantSpace {
				continue
			}
			body := m.PlainText
			if body == "" {
				body = m.Snippet
			}
			if m.SpaceID != "" {
				partsMap[m.SpaceID] = true
			}
			it := NormalizedItem{
				ItemID:                    m.ID,
				ThreadKey:                 thKey,
				Timestamp:                 m.CreateTime,
				Sender:                    m.Sender,
				AuthenticatedParticipants: []string{m.SpaceID},
				SubjectOrRoom:             m.SpaceName,
				Body:                      body,
				IsDirect:                  true,
			}
			items = append(items, it)
			qt.HeadItemID = m.ID
			if qt.SubjectOrRoom == "" {
				qt.SubjectOrRoom = m.SpaceName
			}
			qt.Messages = append(qt.Messages, QuarantinedMessage{
				ItemID:           m.ID,
				Sender:           m.Sender,
				Timestamp:        m.CreateTime,
				Body:             body,
				DKIMOrSPFAligned: true,
			})
			if seenAfter {
				newer = append(newer, it)
				if ownerEmail == "" || !strings.EqualFold(m.Sender, ownerEmail) {
					replied = true
					latestSender = m.Sender
				}
			} else if m.ID == afterID {
				seenAfter = true
			}
		}
		for p := range partsMap {
			qt.AuthenticatedParticipants = append(qt.AuthenticatedParticipants, p)
		}
		sort.Strings(qt.AuthenticatedParticipants)
		return ProtocolResponse{
			ProtocolVersion: ProtocolVersionV1,
			OK:              true,
			Items:           items,
			Thread:          qt,
			Inspection: &ThreadInspection{
				ThreadKey:                 wantSpace,
				HeadItemID:                qt.HeadItemID,
				NewerItems:                newer,
				AuthenticatedParticipants: qt.AuthenticatedParticipants,
				CounterpartyReplied:       replied,
				LatestCounterpartySender:  latestSender,
			},
		}, nil
	}

	// CLI transport path (`transport.kind == "cli"` or configured `cli.path`)
	if accCfg.TransportKind == "cli" || (rc.Manifest.Transport.CLI != nil && strings.TrimSpace(rc.Manifest.Transport.CLI.Path) != "") {
		if req.Op == "poll" || req.Op == "brief" {
			spaces := accCfg.WatchSpaces
			if len(spaces) == 0 && req.Params != nil && req.Params["space"] != "" {
				spaces = []string{req.Params["space"]}
			}
			if len(spaces) == 0 {
				return ProtocolResponse{ProtocolVersion: ProtocolVersionV1, OK: true}, nil
			}
			var allItems []NormalizedItem
			nextCur := req.Cursor
			for _, sp := range spaces {
				subReq := req
				if subReq.Params == nil {
					subReq.Params = map[string]string{}
				}
				subReq.Params["space"] = sp
				subResp, sErr := evalJSONMapConnector(ctx, vaultDir, rc, subReq, allowDesktopScope)
				if sErr != nil {
					return ProtocolResponse{}, sErr
				}
				for i := range subResp.Items {
					subResp.Items[i].AuthenticatedParticipants = []string{sp}
					if subResp.Items[i].ThreadKey == "" {
						subResp.Items[i].ThreadKey = sp
					}
					nextCur = subResp.Items[i].ItemID
				}
				allItems = append(allItems, subResp.Items...)
			}
			return ProtocolResponse{
				ProtocolVersion: ProtocolVersionV1,
				OK:              true,
				NextCursor:      nextCur,
				Items:           allItems,
			}, nil
		}
		// read_quarantined / inspect_thread via CLI get_thread
		if req.Params == nil {
			req.Params = map[string]string{}
		}
		if req.Params["space"] == "" && strings.HasPrefix(req.Params["thread_key"], "spaces/") {
			parts := strings.Split(req.Params["thread_key"], "/")
			if len(parts) >= 2 {
				req.Params["space"] = parts[0] + "/" + parts[1]
			}
		}
		resp, err := evalJSONMapConnector(ctx, vaultDir, rc, req, allowDesktopScope)
		if err != nil {
			return ProtocolResponse{}, err
		}
		sp := req.Params["space"]
		if resp.Thread != nil && sp != "" {
			resp.Thread.AuthenticatedParticipants = []string{sp}
		}
		if resp.Inspection != nil && sp != "" {
			resp.Inspection.AuthenticatedParticipants = []string{sp}
			for _, ni := range resp.Inspection.NewerItems {
				if ownerEmail == "" || !strings.EqualFold(ni.Sender, ownerEmail) {
					resp.Inspection.CounterpartyReplied = true
					resp.Inspection.LatestCounterpartySender = ni.Sender
				}
			}
		}
		return resp, nil
	}

	return ProtocolResponse{ProtocolVersion: ProtocolVersionV1, OK: true}, nil
}

// --- Built-in GCalendar Driver (Low-Sensitivity FreeBusy + Brief Summary) ---

func evalBuiltinGCalendar(ctx context.Context, vaultDir string, rc ResolvedConnector, alias string, _ AccountInstance, req ProtocolRequest, allowDesktopScope bool) (ProtocolResponse, error) {
	if req.Op == "brief" {
		if req.Params == nil {
			req.Params = map[string]string{}
		}
		now := time.Now().UTC()
		if strings.TrimSpace(req.Params["start"]) == "" {
			req.Params["start"] = now.Format(time.RFC3339)
		}
		if strings.TrimSpace(req.Params["end"]) == "" {
			req.Params["end"] = now.Add(24 * time.Hour).Format(time.RFC3339)
		}
	}
	mockFile := filepath.Join(vaultDir, "memory", "gcalendar", "mock.json")
	if envMock := strings.TrimSpace(os.Getenv("HQ_GCAL_MOCK_DIR")); envMock != "" {
		mockFile = filepath.Join(envMock, "mock.json")
	}
	if b, err := os.ReadFile(mockFile); err == nil {
		var mockObj struct {
			Busy   []map[string]interface{} `json:"busy"`
			Events []NormalizedItem         `json:"events"`
		}
		_ = json.Unmarshal(b, &mockObj)
		if req.Op == "query" {
			// Enforce strict start/end projection inside busy[] slots so titles/attendees never leak under taint.
			cleanBusy := make([]map[string]interface{}, 0, len(mockObj.Busy))
			for _, slot := range mockObj.Busy {
				entry := map[string]interface{}{}
				if s, ok := slot["start"]; ok {
					entry["start"] = s
				}
				if e, ok := slot["end"]; ok {
					entry["end"] = e
				}
				cleanBusy = append(cleanBusy, entry)
			}
			return ProtocolResponse{
				ProtocolVersion: ProtocolVersionV1,
				OK:              true,
				QueryData: map[string]interface{}{
					"start": req.Params["start"],
					"end":   req.Params["end"],
					"busy":  cleanBusy,
				},
			}, nil
		}
		return ProtocolResponse{
			ProtocolVersion: ProtocolVersionV1,
			OK:              true,
			Items:           mockObj.Events,
		}, nil
	}
	return evalJSONMapConnector(ctx, vaultDir, rc, req, allowDesktopScope)
}

// --- High-Level Typed Wrappers ---

// Poll executes a single page poll against connector `connID` (`account`).
func Poll(ctx context.Context, vaultDir, connID, account, cursor, pageToken string, maxItems int, allowDesktopScope bool) (ProtocolResponse, error) {
	return Execute(ctx, vaultDir, ProtocolRequest{
		ProtocolVersion: ProtocolVersionV1,
		Op:              "poll",
		Connector:       connID,
		Account:         account,
		Cursor:          cursor,
		PageToken:       pageToken,
		MaxItems:        maxItems,
	}, allowDesktopScope)
}

// InspectThread inspects `threadKey` for items newer than `afterItemID` and returns authenticated participants.
func InspectThread(ctx context.Context, vaultDir, connID, account, threadKey, afterItemID string, allowDesktopScope bool) (ThreadInspection, error) {
	resp, err := Execute(ctx, vaultDir, ProtocolRequest{
		ProtocolVersion: ProtocolVersionV1,
		Op:              "inspect_thread",
		Connector:       connID,
		Account:         account,
		Params: map[string]string{
			"thread_key":    threadKey,
			"after_item_id": afterItemID,
		},
	}, allowDesktopScope)
	if err != nil {
		return ThreadInspection{}, err
	}
	if resp.Inspection == nil {
		return ThreadInspection{ThreadKey: threadKey}, nil
	}
	return *resp.Inspection, nil
}

// ReadQuarantined fetches the normalized `QuarantinedThread` for `threadKey` (caller must persist taint BEFORE rendering!).
func ReadQuarantined(ctx context.Context, vaultDir, connID, account, threadKey string, allowDesktopScope bool) (QuarantinedThread, error) {
	resp, err := Execute(ctx, vaultDir, ProtocolRequest{
		ProtocolVersion: ProtocolVersionV1,
		Op:              "read_quarantined",
		Connector:       connID,
		Account:         account,
		Params: map[string]string{
			"thread_key": threadKey,
		},
	}, allowDesktopScope)
	if err != nil {
		return QuarantinedThread{}, err
	}
	if resp.Thread == nil {
		return QuarantinedThread{Connector: connID, Account: account, ThreadKey: threadKey}, nil
	}
	return *resp.Thread, nil
}

// QueryContext executes a named connector query (`hq watch query`), enforcing `sensitivity == "low"`
// and `project_fields` projection whenever `callerTainted == true`.
func QueryContext(ctx context.Context, vaultDir, connID, account, queryName string, params map[string]string, callerTainted, allowDesktopScope bool) (map[string]interface{}, error) {
	rc, err := Resolve(vaultDir, connID)
	if err != nil {
		return nil, err
	}
	qSpec, ok := rc.Manifest.Queries[strings.TrimSpace(queryName)]
	if !ok {
		return nil, fmt.Errorf("connector %q does not declare query %q", connID, queryName)
	}
	sens := strings.ToLower(strings.TrimSpace(qSpec.Sensitivity))
	if sens == "" {
		sens = "high" // fail-closed default
	}
	if callerTainted && sens != "low" {
		return nil, fmt.Errorf("quarantined session blocked from invoking %s query %q (sensitivity=%s; only sensitivity=\"low\" is permitted)", connID, queryName, sens)
	}
	filteredParams := map[string]string{"query_name": queryName}
	if len(qSpec.AllowedParams) > 0 {
		allowP := make(map[string]bool, len(qSpec.AllowedParams))
		for _, ap := range qSpec.AllowedParams {
			allowP[ap] = true
		}
		for k, v := range params {
			if !allowP[k] {
				return nil, fmt.Errorf("parameter %q is not in allowed_params for query %s.%s", k, connID, queryName)
			}
			filteredParams[k] = v
		}
	} else {
		for k, v := range params {
			filteredParams[k] = v
		}
	}
	resp, err := Execute(ctx, vaultDir, ProtocolRequest{
		ProtocolVersion: ProtocolVersionV1,
		Op:              "query",
		Connector:       connID,
		Account:         account,
		Params:          filteredParams,
	}, allowDesktopScope)
	if err != nil {
		return nil, err
	}
	data := resp.QueryData
	if data == nil {
		data = map[string]interface{}{}
	}
	if len(qSpec.ProjectFields) > 0 {
		projected := make(map[string]interface{}, len(qSpec.ProjectFields))
		for _, f := range qSpec.ProjectFields {
			if val, exists := data[f]; exists {
				projected[f] = val
			}
		}
		return projected, nil
	}
	return data, nil
}

// ApplyOfferPayload executes an approved `OfferApplyEnvelope` piped via `/dev/fd/3` at `hq approve` time.
func ApplyOfferPayload(ctx context.Context, vaultDir string, rawEnvelope []byte, allowDesktopScope bool) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(rawEnvelope))
	dec.DisallowUnknownFields()
	var env OfferApplyEnvelope
	if err := dec.Decode(&env); err != nil {
		return "", fmt.Errorf("invalid offer payload JSON: %w", err)
	}
	rc, err := Resolve(vaultDir, env.Connector)
	if err != nil {
		return "", err
	}
	// Deterministic mock execution path for hermetic E2E tests
	if hasConnectorMock(vaultDir, rc.ID) {
		mockSub := filepath.Join(vaultDir, "memory", rc.ID, "mock")
		if rc.ID == "gmail" && os.Getenv("HQ_GMAIL_MOCK_DIR") != "" {
			mockSub = os.Getenv("HQ_GMAIL_MOCK_DIR")
		} else if rc.ID == "gchat" && os.Getenv("HQ_GCHAT_MOCK_DIR") != "" {
			mockSub = os.Getenv("HQ_GCHAT_MOCK_DIR")
		}
		_ = os.MkdirAll(mockSub, 0o700)
		appliedPath := filepath.Join(mockSub, fmt.Sprintf("applied-offer-%d.json", time.Now().UnixNano()))
		_ = os.WriteFile(appliedPath, rawEnvelope, 0o600)
		return fmt.Sprintf("Applied %s:%s offer (%s) on thread %s", rc.ID, env.Account, env.Action, env.ThreadKey), nil
	}

	verb := strings.TrimSpace(env.Verb)
	if verb == "" {
		if oa, ok := rc.Manifest.Capabilities.Offers[env.Action]; ok && oa.Verb != "" {
			verb = oa.Verb
		} else {
			verb = env.Action
		}
	}
	params := make(map[string]string, len(env.Params)+1)
	for k, v := range env.Params {
		params[k] = v
	}
	if env.ThreadKey != "" && params["thread_key"] == "" {
		params["thread_key"] = env.ThreadKey
	}
	stdout, err := execResolvedCLIVerb(ctx, vaultDir, rc, env.Account, verb, params, nil, []byte(env.Body), allowDesktopScope)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(string(stdout))
	if out == "" {
		out = fmt.Sprintf("Applied %s:%s %s", rc.ID, env.Account, verb)
	}
	return out, nil
}
