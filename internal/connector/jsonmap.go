package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const ProtocolVersionV1 = "v1"

// ProtocolRequest is the JSON stdin envelope for `hq-connector-protocol/v1`.
type ProtocolRequest struct {
	ProtocolVersion string            `json:"protocol_version"`
	Op              string            `json:"op"` // "poll" | "inspect_thread" | "read_quarantined" | "query" | "brief"
	Connector       string            `json:"connector"`
	Account         string            `json:"account"`
	Cursor          string            `json:"cursor,omitempty"`
	PageToken       string            `json:"page_token,omitempty"`
	MaxItems        int               `json:"max_items,omitempty"`
	Params          map[string]string `json:"params,omitempty"`
}

// ProtocolError classifies connector failures into actionable lifecycle transitions.
type ProtocolError struct {
	Code    string `json:"code"` // "auth_expired" | "transient" | "permanent"
	Message string `json:"message"`
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// NormalizedItem represents a single normalized message, ticket, review, or event from any connector.
type NormalizedItem struct {
	ItemID                    string   `json:"item_id"`
	ThreadKey                 string   `json:"thread_key"`
	Timestamp                 string   `json:"timestamp,omitempty"`
	Sender                    string   `json:"sender,omitempty"`
	AuthenticatedParticipants []string `json:"authenticated_participants,omitempty"`
	Mentions                  []string `json:"mentions,omitempty"`
	IsMention                 bool     `json:"is_mention,omitempty"`
	SubjectOrRoom             string   `json:"subject_or_room,omitempty"`
	Permalink                 string   `json:"permalink,omitempty"`
	Body                      string   `json:"body,omitempty"`
	IsDirect                  bool     `json:"is_direct,omitempty"`
	IsAutomated               bool     `json:"is_automated,omitempty"`
	HasSecurityOverride       bool     `json:"has_security_override,omitempty"`
	HasSchedule               bool     `json:"has_schedule,omitempty"`
}

// ThreadInspection summarizes current thread head state for `WaitingOn` auto-resolution and `hq offer` preconditions.
type ThreadInspection struct {
	ThreadKey                 string           `json:"thread_key"`
	HeadItemID                string           `json:"head_item_id"`
	NewerItems                []NormalizedItem `json:"newer_items,omitempty"`
	AuthenticatedParticipants []string         `json:"authenticated_participants,omitempty"`
	CounterpartyReplied       bool             `json:"counterparty_replied,omitempty"`
	LatestCounterpartySender  string           `json:"latest_counterparty_sender,omitempty"`
}

// QuarantinedMessage holds a single message inside a QuarantinedThread.
type QuarantinedMessage struct {
	ItemID           string `json:"item_id"`
	Sender           string `json:"sender"`
	To               string `json:"to,omitempty"`
	Cc               string `json:"cc,omitempty"`
	Timestamp        string `json:"timestamp,omitempty"`
	Body             string `json:"body"`
	DKIMOrSPFAligned bool   `json:"dkim_or_spf_aligned,omitempty"`
}

// QuarantinedThread holds a normalized thread ready for nonce-fenced rendering in `hq watch read`.
type QuarantinedThread struct {
	Connector                 string               `json:"connector"`
	Account                   string               `json:"account"`
	ThreadKey                 string               `json:"thread_key"`
	HeadItemID                string               `json:"head_item_id"`
	SubjectOrRoom             string               `json:"subject_or_room,omitempty"`
	AuthenticatedParticipants []string             `json:"authenticated_participants,omitempty"`
	Messages                  []QuarantinedMessage `json:"messages"`
}

// ProtocolResponse is the JSON stdout envelope for `hq-connector-protocol/v1`.
type ProtocolResponse struct {
	ProtocolVersion string                 `json:"protocol_version"`
	OK              bool                   `json:"ok"`
	NextCursor      string                 `json:"next_cursor,omitempty"`
	NextPageToken   string                 `json:"next_page_token,omitempty"`
	Items           []NormalizedItem       `json:"items,omitempty"`
	Inspection      *ThreadInspection      `json:"inspection,omitempty"`
	Thread          *QuarantinedThread     `json:"thread,omitempty"`
	QueryData       map[string]interface{} `json:"query_data,omitempty"`
	Error           *ProtocolError         `json:"error,omitempty"`
}

// EvalJSONPointer evaluates an RFC 6901 JSON-Pointer (`""`, `"/a/b/0"`, unescaping `~1` -> `/` and `~0` -> `~`).
func EvalJSONPointer(doc interface{}, ptr string) (interface{}, bool) {
	if ptr == "" {
		return doc, doc != nil
	}
	if strings.HasPrefix(ptr, "const:") {
		return strings.TrimPrefix(ptr, "const:"), true
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, false
	}
	parts := strings.Split(ptr[1:], "/")
	cur := doc
	for _, rawSeg := range parts {
		seg := strings.ReplaceAll(rawSeg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch node := cur.(type) {
		case map[string]interface{}:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []interface{}:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, cur != nil
}

func stringifyJSONVal(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case []interface{}:
		parts := make([]string, 0, len(x))
		for _, elem := range x {
			if s := stringifyJSONVal(elem); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func stringSliceFromJSONVal(v interface{}) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []interface{}:
		var out []string
		for _, elem := range x {
			switch sub := elem.(type) {
			case string:
				if s := strings.TrimSpace(sub); s != "" {
					out = append(out, s)
				}
			case map[string]interface{}:
				for _, k := range []string{"email", "address", "id", "name", "login"} {
					if s := strings.TrimSpace(stringifyJSONVal(sub[k])); s != "" {
						out = append(out, s)
						break
					}
				}
			}
		}
		return out
	case string:
		var out []string
		for _, p := range strings.Split(x, ",") {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func boolFromJSONVal(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "true" || s == "1" || s == "yes"
	case float64:
		return x != 0
	default:
		return false
	}
}

func applyTransforms(val string, transforms []string) string {
	cur := val
	for _, t := range transforms {
		t = strings.TrimSpace(t)
		switch {
		case t == "trim":
			cur = strings.TrimSpace(cur)
		case t == "lower":
			cur = strings.ToLower(cur)
		case t == "upper":
			cur = strings.ToUpper(cur)
		case t == "rfc3339":
			if sec, err := strconv.ParseInt(strings.TrimSpace(cur), 10, 64); err == nil {
				if sec > 1e12 {
					cur = time.UnixMilli(sec).UTC().Format(time.RFC3339)
				} else {
					cur = time.Unix(sec, 0).UTC().Format(time.RFC3339)
				}
			} else if parsed, pErr := time.Parse(time.RFC1123Z, cur); pErr == nil {
				cur = parsed.UTC().Format(time.RFC3339)
			} else if parsed2, pErr2 := time.Parse(time.RFC3339Nano, cur); pErr2 == nil {
				cur = parsed2.UTC().Format(time.RFC3339)
			}
		case strings.HasPrefix(t, "const:"):
			cur = strings.TrimPrefix(t, "const:")
		case strings.HasPrefix(t, "prefix:"):
			if cur != "" {
				cur = strings.TrimPrefix(t, "prefix:") + cur
			}
		}
	}
	return cur
}

func matchesPredicates(itemObj interface{}, preds []PredicateSpec) bool {
	for _, p := range preds {
		val, ok := EvalJSONPointer(itemObj, p.Pointer)
		s := strings.TrimSpace(stringifyJSONVal(val))
		switch strings.ToLower(strings.TrimSpace(p.Op)) {
		case "eq", "":
			if !ok || s != p.Value {
				return false
			}
		case "neq":
			if ok && s == p.Value {
				return false
			}
		case "contains":
			if !ok || !strings.Contains(strings.ToLower(s), strings.ToLower(p.Value)) {
				return false
			}
		case "nonempty":
			if !ok || s == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// MapCLIOutput maps raw JSON (object/array) or NDJSON bytes into NormalizedItems + pagination cursors using JSONMapSpec.
func MapCLIOutput(raw []byte, spec JSONMapSpec) ([]NormalizedItem, string, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		return nil, "", "", nil
	}

	var rawItems []interface{}
	nextCursor := ""
	nextPage := ""

	isNDJSON := strings.EqualFold(strings.TrimSpace(spec.Format), "ndjson")
	if !isNDJSON {
		var root interface{}
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&root); err == nil {
			// Check if there is trailing data (indicating NDJSON stream without explicit format="ndjson")
			var extra interface{}
			if dec.Decode(&extra) != io.EOF {
				isNDJSON = true
			} else {
				if spec.NextCursorPointer != "" {
					if v, ok := EvalJSONPointer(root, spec.NextCursorPointer); ok {
						nextCursor = strings.TrimSpace(stringifyJSONVal(v))
					}
				}
				if spec.NextPagePointer != "" {
					if v, ok := EvalJSONPointer(root, spec.NextPagePointer); ok {
						nextPage = strings.TrimSpace(stringifyJSONVal(v))
					}
				}
				targetNode := root
				if spec.ItemsPointer != "" {
					if sub, ok := EvalJSONPointer(root, spec.ItemsPointer); ok {
						targetNode = sub
					} else {
						targetNode = nil
					}
				}
				switch arr := targetNode.(type) {
				case []interface{}:
					rawItems = arr
				case map[string]interface{}:
					rawItems = []interface{}{arr}
				}
			}
		} else {
			isNDJSON = true
		}
	}

	if isNDJSON {
		scanner := bufio.NewScanner(bytes.NewReader(trimmed))
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1 MiB max line
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var obj interface{}
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil {
				return nil, "", "", fmt.Errorf("invalid NDJSON line: %w", err)
			}
			// Skip Gerrit-style stats footer objects without item_id when item_id pointer is set
			if spec.ItemID != "" {
				if v, ok := EvalJSONPointer(obj, spec.ItemID); !ok || strings.TrimSpace(stringifyJSONVal(v)) == "" {
					continue
				}
			}
			rawItems = append(rawItems, obj)
		}
		if err := scanner.Err(); err != nil {
			return nil, "", "", fmt.Errorf("scan NDJSON output: %w", err)
		}
	}

	readStr := func(obj interface{}, fieldKey, ptr string) string {
		if ptr == "" {
			return ""
		}
		v, ok := EvalJSONPointer(obj, ptr)
		if !ok {
			return ""
		}
		s := stringifyJSONVal(v)
		if tfs := spec.Transforms[fieldKey]; len(tfs) > 0 {
			s = applyTransforms(s, tfs)
		}
		return strings.TrimSpace(s)
	}

	var out []NormalizedItem
	for _, elem := range rawItems {
		if !matchesPredicates(elem, spec.Predicates) {
			continue
		}
		itemID := readStr(elem, "item_id", spec.ItemID)
		if itemID == "" {
			continue
		}
		threadKey := readStr(elem, "thread_key", spec.ThreadKey)
		if threadKey == "" {
			threadKey = itemID
		}
		item := NormalizedItem{
			ItemID:        itemID,
			ThreadKey:     threadKey,
			Timestamp:     readStr(elem, "timestamp", spec.Timestamp),
			Sender:        readStr(elem, "sender", spec.Sender),
			SubjectOrRoom: readStr(elem, "subject_or_room", spec.SubjectOrRoom),
			Permalink:     readStr(elem, "permalink", spec.Permalink),
			Body:          readStr(elem, "body", spec.Body),
		}
		if spec.Participants != "" {
			if v, ok := EvalJSONPointer(elem, spec.Participants); ok {
				item.AuthenticatedParticipants = stringSliceFromJSONVal(v)
			}
		}
		if spec.Mentions != "" {
			if v, ok := EvalJSONPointer(elem, spec.Mentions); ok {
				item.Mentions = stringSliceFromJSONVal(v)
				item.IsMention = len(item.Mentions) > 0
			}
		}
		if spec.IsDirect != "" {
			if v, ok := EvalJSONPointer(elem, spec.IsDirect); ok {
				item.IsDirect = boolFromJSONVal(v)
			}
		}
		if spec.IsAutomated != "" {
			if v, ok := EvalJSONPointer(elem, spec.IsAutomated); ok {
				item.IsAutomated = boolFromJSONVal(v)
			}
		}
		if spec.HasSchedule != "" {
			if v, ok := EvalJSONPointer(elem, spec.HasSchedule); ok {
				item.HasSchedule = boolFromJSONVal(v)
			}
		}
		out = append(out, item)
	}
	return out, nextCursor, nextPage, nil
}

// RunJSONMapDriverStdio implements `hq connector-driver jsonmap` (`hq-connector-protocol/v1` over stdin/stdout).
func RunJSONMapDriverStdio(ctx context.Context, vaultDir string, r io.Reader, w io.Writer) error {
	var req ProtocolRequest
	dec := json.NewDecoder(io.LimitReader(r, 1024*1024))
	if err := dec.Decode(&req); err != nil {
		return writeProtocolError(w, "permanent", "invalid protocol/v1 request JSON: "+err.Error())
	}
	rc, err := Resolve(vaultDir, req.Connector)
	if err != nil {
		return writeProtocolError(w, "permanent", err.Error())
	}
	resp, err := evalJSONMapConnector(ctx, vaultDir, rc, req, false)
	if err != nil {
		return writeProtocolError(w, classifyErrCode(err), err.Error())
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}

func writeProtocolError(w io.Writer, code, msg string) error {
	resp := ProtocolResponse{
		ProtocolVersion: ProtocolVersionV1,
		OK:              false,
		Error:           &ProtocolError{Code: code, Message: msg},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}
