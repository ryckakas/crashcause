package ai

import (
	"strings"
	"testing"
)

// javaStackTrace and goPanicTrace must survive redaction byte-identically:
// they are exactly the evidence the AI summary is supposed to interpret.
const javaStackTrace = `2026-08-02T09:14:22.114Z ERROR [billing-api] o.s.b.w.s.s.ErrorPageFilter : Forwarding to error page
java.lang.IllegalStateException: Failed to obtain JDBC Connection
	at org.springframework.jdbc.datasource.DataSourceUtils.getConnection(DataSourceUtils.java:82)
	at org.springframework.security.web.authentication.UsernamePasswordAuthenticationFilter.doFilter(UsernamePasswordAuthenticationFilter.java:334)
	at com.example.billing.InvoiceRepository.findAll(InvoiceRepository.java:117)
	at com.example.billing.BillingApplication.main(BillingApplication.java:19)
Caused by: org.postgresql.util.PSQLException: Connection to db.prod.svc.cluster.local:5432 refused. Check that the hostname and port are correct.
	... 42 common frames omitted`

const goPanicTrace = `panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1b2c]

goroutine 1 [running]:
main.(*Server).handle(0xc0000b4000, {0x7f9c20, 0xc000112000})
	/src/cmd/app/server.go:142 +0x2f
main.main()
	/src/cmd/app/main.go:31 +0x105
exit status 2`

func TestRedactorBuiltinPatterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bearer authorization header",
			in:   `Authorization: Bearer abc123DEFghi456`,
			want: `Authorization: Bearer [REDACTED]`,
		},
		{
			name: "basic authorization header lowercase",
			in:   `authorization: basic dXNlcjpodW50ZXIy`,
			want: `authorization: basic [REDACTED]`,
		},
		{
			name: "authorization header in json",
			in:   `{"Authorization": "Bearer abc123DEFghi456"}`,
			want: `{"Authorization": "Bearer [REDACTED]"}`,
		},
		{
			name: "proxy authorization header",
			in:   `Proxy-Authorization: Basic Zm9vOmJhcg==`,
			want: `Proxy-Authorization: Basic [REDACTED]`,
		},
		{
			name: "opaque authorization header",
			in:   `Authorization: abcdef0123456789`,
			want: `Authorization: [REDACTED]`,
		},
		{
			name: "api_key equals",
			in:   `starting with api_key=pk-test-0123456789abcdef`,
			want: `starting with api_key=[REDACTED]`,
		},
		{
			name: "prefixed api key colon",
			in:   `x-api-key: 0123456789abcdef`,
			want: `x-api-key: [REDACTED]`,
		},
		{
			name: "password colon",
			in:   `password: hunter2`,
			want: `password: [REDACTED]`,
		},
		{
			name: "env style password",
			in:   `DB_PASSWORD=hunter2 DB_HOST=db.prod.svc`,
			want: `DB_PASSWORD=[REDACTED] DB_HOST=db.prod.svc`,
		},
		{
			name: "quoted json token keeps quotes",
			in:   `{"token": "abc.def.ghi", "user": "svc-billing"}`,
			want: `{"token": "[REDACTED]", "user": "svc-billing"}`,
		},
		{
			name: "single quoted secret",
			in:   `secret='s3cr3t value'`,
			want: `secret='[REDACTED]'`,
		},
		{
			name: "aws access key id",
			in:   `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE region=eu-central-1`,
			want: `AWS_ACCESS_KEY_ID=[REDACTED] region=eu-central-1`,
		},
		{
			name: "bare aws access key id in prose",
			in:   `credential AKIAIOSFODNN7EXAMPLE is not authorized`,
			want: `credential [REDACTED] is not authorized`,
		},
		{
			name: "aws secret access key",
			in:   `aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
			want: `aws_secret_access_key=[REDACTED]`,
		},
		{
			name: "jwt",
			in:   `rejecting eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c for /v1/invoices`,
			want: `rejecting [REDACTED] for /v1/invoices`,
		},
		{
			name: "url credentials keep host and port",
			in:   `postgres://user:hunter2@db.prod:5432/x`,
			want: `postgres://***@db.prod:5432/x`,
		},
		{
			name: "url credentials inside a log line",
			in:   `FATAL: could not connect to postgres://app:s3cr3t@db.prod.svc.cluster.local:5432/billing?sslmode=require`,
			want: `FATAL: could not connect to postgres://***@db.prod.svc.cluster.local:5432/billing?sslmode=require`,
		},
		{
			name: "url in a secret-named env var still keeps host and port",
			in:   `DATABASE_PASSWORD_URL=amqp://svc:p4ss@rabbit.prod:5672/vhost`,
			want: `DATABASE_PASSWORD_URL=amqp://***@rabbit.prod:5672/vhost`,
		},
		{
			name: "url without credentials untouched",
			in:   `dial tcp: http://db.prod.svc:5432/healthz timed out`,
			want: `dial tcp: http://db.prod.svc:5432/healthz timed out`,
		},
		{
			name: "bare ip survives by default",
			in:   `dial tcp 10.2.3.4:5432: connect: connection refused`,
			want: `dial tcp 10.2.3.4:5432: connect: connection refused`,
		},
		{
			name: "postgres password authentication failure preserved",
			in:   `FATAL: password authentication failed for user "billing"`,
			want: `FATAL: password authentication failed for user "billing"`,
		},
		{
			name: "kubernetes secret reference preserved",
			in:   `Error: secret "db-credentials" not found; secretName: db-credentials`,
			want: `Error: secret "db-credentials" not found; secretName: db-credentials`,
		},
		{
			name: "exception names and file paths preserved",
			in:   `at com.example.auth.TokenService.verify(TokenService.java:88)`,
			want: `at com.example.auth.TokenService.verify(TokenService.java:88)`,
		},
		{
			name: "java stack trace survives byte identical",
			in:   javaStackTrace,
			want: javaStackTrace,
		},
		{
			name: "go panic survives byte identical",
			in:   goPanicTrace,
			want: goPanicTrace,
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
	}

	r, err := NewRedactor(RedactorOptions{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Redact(tc.in); got != tc.want {
				t.Errorf("Redact()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestRedactorRedactIPs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "ip scrubbed but port survives",
			in:   `dial tcp 10.2.3.4:5432: connect: connection refused`,
			want: `dial tcp [REDACTED-IP]:5432: connect: connection refused`,
		},
		{
			name: "ip inside url host position",
			in:   `postgres://user:hunter2@10.2.3.4:5432/x`,
			want: `postgres://***@[REDACTED-IP]:5432/x`,
		},
		{
			name: "hostname untouched",
			in:   `dial tcp db.prod.svc:5432: connection refused`,
			want: `dial tcp db.prod.svc:5432: connection refused`,
		},
	}

	r, err := NewRedactor(RedactorOptions{RedactIPs: true})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Redact(tc.in); got != tc.want {
				t.Errorf("Redact()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestRedactorExtraPatterns(t *testing.T) {
	r, err := NewRedactor(RedactorOptions{
		ExtraPatterns: []string{
			"# org specific identifiers",
			"",
			`ACME-[0-9]{6}`,
			`(?i)internal-ref-[a-f0-9]{8}`,
			"   ",
		},
	})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	in := `customer ACME-123456 failed, see INTERNAL-REF-deadbeef and hostname db.prod.svc`
	want := `customer [REDACTED] failed, see [REDACTED] and hostname db.prod.svc`
	if got := r.Redact(in); got != want {
		t.Errorf("Redact()\n got: %q\nwant: %q", got, want)
	}

	patterns := r.Patterns()
	if !containsSubstring(patterns, `ACME-[0-9]{6}`) {
		t.Errorf("Patterns() does not document the extra pattern: %v", patterns)
	}
}

func TestNewRedactorInvalidExtraPattern(t *testing.T) {
	r, err := NewRedactor(RedactorOptions{ExtraPatterns: []string{`valid`, `[unclosed`}})
	if err == nil {
		t.Fatalf("NewRedactor: expected an error for an invalid regex, got %v", r)
	}
	if r != nil {
		t.Errorf("NewRedactor: expected a nil Redactor alongside the error, got %v", r)
	}
	if !strings.Contains(err.Error(), "[unclosed") {
		t.Errorf("error should name the offending pattern, got %q", err.Error())
	}
}

func TestRedactLines(t *testing.T) {
	r, err := NewRedactor(RedactorOptions{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	in := []string{
		`Connecting to postgres://app:s3cr3t@db.prod:5432/billing`,
		`api_key=pk-test-abcdef`,
		`	at com.example.billing.InvoiceRepository.findAll(InvoiceRepository.java:117)`,
	}
	want := []string{
		`Connecting to postgres://***@db.prod:5432/billing`,
		`api_key=[REDACTED]`,
		`	at com.example.billing.InvoiceRepository.findAll(InvoiceRepository.java:117)`,
	}

	got := r.RedactLines(in)
	if len(got) != len(want) {
		t.Fatalf("RedactLines returned %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
	if in[1] != `api_key=pk-test-abcdef` {
		t.Errorf("RedactLines mutated its input: %q", in[1])
	}
	if r.RedactLines(nil) != nil {
		t.Error("RedactLines(nil) should return nil")
	}
}

func TestRedactorPatternsDocumentsIPBehavior(t *testing.T) {
	off, err := NewRedactor(RedactorOptions{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	on, err := NewRedactor(RedactorOptions{RedactIPs: true})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	if len(off.Patterns()) < 6 {
		t.Errorf("Patterns() should describe every built-in rule, got %d", len(off.Patterns()))
	}
	if !containsSubstring(off.Patterns(), "NOT redacted by default") {
		t.Errorf("Patterns() must state that IPs are kept by default: %v", off.Patterns())
	}
	if !containsSubstring(on.Patterns(), redactedIPPlaceholder) {
		t.Errorf("Patterns() must state that IPs are scrubbed when enabled: %v", on.Patterns())
	}
	for _, p := range off.Patterns() {
		if strings.TrimSpace(p) == "" {
			t.Error("Patterns() returned an empty description")
		}
	}
}

func TestRedactorNilReceiver(t *testing.T) {
	var r *Redactor
	if got, want := r.Redact(`password: hunter2`), `password: [REDACTED]`; got != want {
		t.Errorf("nil Redactor.Redact()\n got: %q\nwant: %q", got, want)
	}
	if got := r.RedactLines([]string{`token=abc`}); len(got) != 1 || got[0] != `token=[REDACTED]` {
		t.Errorf("nil Redactor.RedactLines() = %v", got)
	}
	if len(r.Patterns()) == 0 {
		t.Error("nil Redactor.Patterns() should still describe the built-ins")
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
