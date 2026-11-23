package store

import "testing"

func TestTruncateRunesAndWithSuffix(t *testing.T) {
	if got := TruncateRunes("hello", 0); got != "" {
		t.Fatalf("TruncateRunes(max=0) = %q, want empty", got)
	}
	if got := TruncateRunes("hello", -1); got != "" {
		t.Fatalf("TruncateRunes(max=-1) = %q, want empty", got)
	}
	if got := TruncateRunes("hello", 5); got != "hello" {
		t.Fatalf("TruncateRunes(exact) = %q, want hello", got)
	}
	if got := TruncateRunes("hello world", 8); got != "hello..." {
		t.Fatalf("TruncateRunes(8) = %q, want hello...", got)
	}
	if got := TruncateRunes("hello", 2); got != "he" {
		t.Fatalf("TruncateRunes(2) = %q, want he", got)
	}
	if got := TruncateRunes("你好世界测试", 5); got != "你好..." {
		t.Fatalf("TruncateRunes(unicode) = %q, want 你好...", got)
	}
	if got := TruncateRunesWithSuffix("abcdef", 5, "…"); got != "abcd…" {
		t.Fatalf("TruncateRunesWithSuffix(unicode ellipsis) = %q, want abcd…", got)
	}
}

func TestShortConvID(t *testing.T) {
	if got := ShortConvID("   "); got != "unknown" {
		t.Fatalf("ShortConvID(blank) = %q, want unknown", got)
	}
	if got := ShortConvID("abc123"); got != "abc123" {
		t.Fatalf("ShortConvID(short) = %q, want abc123", got)
	}
	if got := ShortConvID(" 1234567890abcdef "); got != "12345678" {
		t.Fatalf("ShortConvID(long) = %q, want 12345678", got)
	}
}

func TestDefangURLs(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   \t\n ", ""},
		{"plain text", "plain text"},
		{"visit http://a.example and https://b.example", "visit hxxp://a.example and hxxps://b.example"},
		{"mixed HTTP://A.EX and HTTPS://B.EX", "mixed hxxp://A.EX and hxxps://B.EX"},
		{"already hxxps://safe.example and hxxp://safe.example", "already hxxps://safe.example and hxxp://safe.example"},
		{"_https://evil.example/track_", "_hxxps://evil.example/track_"},
		{"_http://evil.example/track_", "_hxxp://evil.example/track_"},
		{"*https://evil.example/track*", "*hxxps://evil.example/track*"},
		{"shttp://x.example and foohttps://y.example", "shttp://x.example and foohttps://y.example"},
		{"https://a.com/?u=http://b.com", "hxxps://a.com/?u=hxxp://b.com"},
		{"url=https://a.example (http://b.example) <https://c.example>", "url=hxxps://a.example (hxxp://b.example) <hxxps://c.example>"},
		{"https://a.com/https://b.com", "hxxps://a.com/hxxps://b.com"},
		{"http://http://evil.example and https://https://evil.example", "hxxp://hxxp://evil.example and hxxps://hxxps://evil.example"},
		{"https://http://evil.example and _http://_https://", "hxxps://hxxp://evil.example and _hxxp://_hxxps://"},
		{"shttp://https://evil.example", "shttp://hxxps://evil.example"},
		{"你好https://evil.example", "你好hxxps://evil.example"},
	}
	for _, tc := range cases {
		if got := DefangURLs(tc.in); got != tc.want {
			t.Fatalf("DefangURLs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
