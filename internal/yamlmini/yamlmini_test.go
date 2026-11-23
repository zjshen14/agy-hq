package yamlmini

import (
	"reflect"
	"testing"
)

func TestLinesAndLeadingIndent(t *testing.T) {
	lines := Lines("a: 1\r\n  b: 2\n\tc: 3")
	want := []string{"a: 1", "  b: 2", "\tc: 3"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("Lines() = %#v, want %#v", lines, want)
	}
	if got := LeadingIndent("  b: 2"); got != 2 {
		t.Fatalf("LeadingIndent(2 spaces) = %d, want 2", got)
	}
	if got := LeadingIndent("\tc: 3"); got != 2 {
		t.Fatalf("LeadingIndent(1 tab) = %d, want 2", got)
	}
	if got := LeadingIndent(" \tc: 3"); got != 3 {
		t.Fatalf("LeadingIndent(space+tab) = %d, want 3", got)
	}
	if got := LeadingIndent(""); got != 0 {
		t.Fatalf("LeadingIndent(empty) = %d, want 0", got)
	}
}

func TestStripCommentQuoteAware(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"full line comment", "  # comment", ""},
		{"inline comment", "mode: auto # keep default", "mode: auto"},
		{"hash in double quotes", `regex: "(?i)#1 rule" # trailing`, `regex: "(?i)#1 rule"`},
		{"hash in single quotes", `pattern: 'foo ''#bar'' baz' # note`, `pattern: 'foo ''#bar'' baz'`},
		{"url fragment preserved", "url: https://example.com#section # link", "url: https://example.com#section"},
		{"apostrophe in unquoted text", "description: Alice's rule # note", "description: Alice's rule"},
		{"escaped quote in double quotes", `cmd: "echo \"#stay\"" # end`, `cmd: "echo \"#stay\""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripComment(tc.in); got != tc.want {
				t.Fatalf("StripComment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnquoteScalar(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`  plain  `, `plain`},
		{`'it''s ok'`, `it's ok`},
		{`"line1\n\t\"quoted\"\\end\/"`, "line1\n\t\"quoted\"\\end/"},
		{`"(?i)\bfoo\s+\d+"`, `(?i)\bfoo\s+\d+`},
		{`"'a'"`, `'a'`},
		{`"unclosed`, `"unclosed`},
	}
	for _, tc := range cases {
		if got := UnquoteScalar(tc.in); got != tc.want {
			t.Fatalf("UnquoteScalar(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitKV(t *testing.T) {
	cases := []struct {
		line   string
		wantK  string
		wantV  string
		wantOK bool
	}{
		{"key: value", "key", "value", true},
		{"section:", "section", "", true},
		{`"en": "English"`, `"en"`, `"English"`, true},
		{`url: https://example.com:8443/path`, `url`, `https://example.com:8443/path`, true},
		{`cmd: "a: b" # comment`, `cmd`, `"a: b" # comment`, true},
		{`description: Alice's rule: ok`, `description`, `Alice's rule: ok`, true},
		{`foo:bar`, "", "", false},
		{`: missing_key`, "", "", false},
	}
	for _, tc := range cases {
		k, v, ok := SplitKV(tc.line)
		if k != tc.wantK || v != tc.wantV || ok != tc.wantOK {
			t.Fatalf("SplitKV(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.line, k, v, ok, tc.wantK, tc.wantV, tc.wantOK)
		}
	}
}

func TestParseInlineList(t *testing.T) {
	if got, ok := ParseInlineList("[]"); !ok || got == nil || len(got) != 0 {
		t.Fatalf("ParseInlineList([]) = (%#v, %v), want non-nil empty slice", got, ok)
	}
	if got, ok := ParseInlineList(`["a, b", 'c''d', e, ]`); !ok || !reflect.DeepEqual(got, []string{"a, b", "c'd", "e"}) {
		t.Fatalf("ParseInlineList(quoted) = (%#v, %v)", got, ok)
	}
	if got, ok := ParseInlineList(`[Alice's, Bob's]`); !ok || !reflect.DeepEqual(got, []string{"Alice's", "Bob's"}) {
		t.Fatalf("ParseInlineList(apostrophes) = (%#v, %v)", got, ok)
	}
	if got, ok := ParseInlineList("exit 2"); ok || got != nil {
		t.Fatalf("ParseInlineList(scalar) = (%#v, %v), want (nil, false)", got, ok)
	}
}

func TestTrimListItem(t *testing.T) {
	if rest, ok := TrimListItem("- id: rule1"); !ok || rest != "id: rule1" {
		t.Fatalf("TrimListItem(- id) = (%q, %v)", rest, ok)
	}
	if rest, ok := TrimListItem("-\titem"); !ok || rest != "item" {
		t.Fatalf("TrimListItem(-\\t) = (%q, %v)", rest, ok)
	}
	if rest, ok := TrimListItem("-"); !ok || rest != "" {
		t.Fatalf("TrimListItem(-) = (%q, %v)", rest, ok)
	}
	if rest, ok := TrimListItem("-foo"); ok || rest != "" {
		t.Fatalf("TrimListItem(-foo) = (%q, %v), want false", rest, ok)
	}
}

func TestSplitTopLevelBlocks(t *testing.T) {
	input := "# header comment\r\nfirst: # note\r\n  # nested comment\r\n  a: 1\r\n\r\nsecond:\r\n  b: 2\r\n"
	blocks := SplitTopLevelBlocks(input)
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2 (%#v)", len(blocks), blocks)
	}
	if blocks[0].Key != "first" || blocks[0].Content != "first: # note\n  # nested comment\n  a: 1" {
		t.Fatalf("block[0] = %#v", blocks[0])
	}
	if blocks[1].Key != "second" || blocks[1].Content != "second:\n  b: 2" {
		t.Fatalf("block[1] = %#v", blocks[1])
	}
}
