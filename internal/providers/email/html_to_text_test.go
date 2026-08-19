package email

import "testing"

func TestHTMLToPlainText(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"empty input", "", ""},
		{
			name: "simple paragraph",
			html: "<p>Hello there</p>",
			want: "Hello there",
		},
		{
			name: "br becomes newline",
			html: "Line one<br>Line two<br/>Line three",
			want: "Line one\nLine two\nLine three",
		},
		{
			name: "strips script and style blocks entirely",
			html: "<style>.x{color:red}</style><p>Visible</p><script>alert(1)</script>",
			want: "Visible",
		},
		{
			name: "unescapes HTML entities",
			html: "<p>Ts&amp;Cs apply &mdash; 100% &lt;safe&gt;</p>",
			want: "Ts&Cs apply — 100% <safe>",
		},
		{
			name: "collapses excessive blank lines from block tags",
			html: "<div>A</div><div></div><div></div><div>B</div>",
			want: "A\n\nB",
		},
		{
			name: "real-world OTP template shape",
			html: `<div class="header"><img src="logo.png"/></div><p>Use the code below:</p><div class="otp-box">123456</div><p>Expires in 5 minutes.</p>`,
			want: "Use the code below:\n123456\nExpires in 5 minutes.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := HTMLToPlainText(c.html)
			if got != c.want {
				t.Errorf("HTMLToPlainText(%q) = %q, want %q", c.html, got, c.want)
			}
		})
	}
}
