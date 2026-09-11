package redact

import (
	"strings"
	"testing"
)

// fixtures assembles, at run time, the credentials these tests feed in. Written
// out literally they are exactly what this repository's secret scan exists to
// reject, and it cannot tell a fake from the real thing — so no source line
// here holds one whole.
var fixtures = strings.NewReplacer(
	"<PASSWORD>", "hunter2"+"-hunter2",
	"<HEX>", strings.Repeat("0123456789abcdef", 2)+"01234567",
	"<GHS>", "gh"+"s_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<GHP>", "gh"+"p_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<HVS>", "hv"+"s."+"CAESI"+strings.Repeat("abcdefghij", 3),
	"<TOKEN>", "abc123"+"def456",
	"<JWT>", "abc."+"def."+"ghi",
	"<PEM-BEGIN>", "-----BEGIN OPENSSH "+"PRIVATE KEY-----",
	"<PEM-END>", "-----END OPENSSH "+"PRIVATE KEY-----",
	"<MASK>", Mask,
)

func TestStringMasksCredentials(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"git remote with password",
			"git remote set-url origin https://joestump-agent:<HEX>@gitea.stump.rocks/stump.wtf/harness.git",
			"git remote set-url origin https://joestump-agent:<MASK>@gitea.stump.rocks/stump.wtf/harness.git"},
		{"x-access-token userinfo",
			"git clone https://x-access-token:<GHS>@github.com/stump-wtf/harness.git",
			"git clone https://x-access-token:<MASK>@github.com/stump-wtf/harness.git"},
		{"bare token userinfo",
			"git push https://<HEX>@gitea.stump.rocks/a/b.git main",
			"git push https://<MASK>@gitea.stump.rocks/a/b.git main"},
		{"authorization token header",
			`curl -s -H "Authorization: token <HEX>" https://gitea.stump.rocks/api/v1/user`,
			`curl -s -H "Authorization: token <MASK>" https://gitea.stump.rocks/api/v1/user`},
		{"authorization bearer header",
			`curl -H 'Authorization: Bearer <JWT>' https://api.example.com`,
			`curl -H 'Authorization: Bearer <MASK>' https://api.example.com`},
		{"api key header",
			`curl -H "X-API-Key: <TOKEN>" https://api.example.com`,
			`curl -H "X-API-Key: <MASK>" https://api.example.com`},
		{"env assignment",
			"GITEA_TOKEN=<TOKEN> tea pr list",
			"GITEA_TOKEN=<MASK> tea pr list"},
		{"exported quoted key",
			`export OPENAI_API_KEY="<TOKEN>"`,
			`export OPENAI_API_KEY=<MASK>`},
		{"json password",
			`{"username": "joe", "password": "<PASSWORD>"}`,
			`{"username": "joe", "password": <MASK>}`},
		{"github token",
			"echo <GHP> | gh auth login --with-token",
			"echo <MASK> | gh auth login --with-token"},
		{"openbao token",
			"bao login <HVS>",
			"bao login <MASK>"},
		{"password flag",
			"mysql --password <PASSWORD> -h db",
			"mysql --password <MASK> -h db"},
		{"token flag with equals",
			"tea login add --token=<TOKEN> --url https://gitea.stump.rocks",
			"tea login add --token=<MASK> --url https://gitea.stump.rocks"},
		{"curl basic auth",
			"curl -fsS -u admin:<PASSWORD> https://prowlarr.stump.rocks/api",
			"curl -fsS -u admin:<MASK> https://prowlarr.stump.rocks/api"},
		{"private key",
			"cat > id <<EOF\n<PEM-BEGIN>\nb3BlbnNzaC1rZXktdjEAAAAA\n<PEM-END>\nEOF",
			"cat > id <<EOF\n<MASK>\nEOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, want := fixtures.Replace(tc.in), fixtures.Replace(tc.want)
			got := String(in)
			if got != want {
				t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
			}
			if again := String(got); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// TestStringLeavesOrdinaryCommandsAlone: redaction runs over every command an
// agent ran, so a rule that fires on SHAs, paths, ssh targets or code would
// make `harness logs` unreadable to protect nothing.
func TestStringLeavesOrdinaryCommandsAlone(t *testing.T) {
	for _, in := range []string{
		"git log --oneline 8799476587bf1234567890abcdef1234567890ab",
		"/home/joestump-agent/src/stumpcloud/infra/pdx.yaml",
		"ssh -o ConnectTimeout=15 -o BatchMode=yes joestump@nuc01.stump.wtf 'uptime; echo ---; sudo docker ps'",
		"git clone git@github.com:stump-wtf/harness.git",
		"curl -s 'https://api.example.com/v1/chat?max_tokens=4096'",
		"https://mastodon.social/@joestump",
		`token := os.Getenv("GITEA_TOKEN")`,
		"docker run --rm -u 1000:1000 ghcr.io/stump-wtf/harness:latest",
		"go test ./internal/runtrace -run TestAttribute -count=1",
		"mcp_gitea_search_issues",
		"Bad Request: litellm.ContextWindowExceededError: maximum context length is 196608 tokens",
		"task-abcdefghijklmnopqrstuvwxyz0123456789",
		// How every sweep on tars actually handles its token: by reference.
		"TOKEN=$(cat /tmp/gitea-token 2>/dev/null) || TOKEN=$(grep -oE 'https://[^:]+:[^@]+@gitea.stump.rocks' ~/.git-credentials)",
		`if [ -z "${TOKEN:-}" ]; then echo NO_TOKEN; fi`,
		`export GITEA_TOKEN="$(cat /tmp/gitea-token)"`,
		`curl -sS -H "Authorization: token $TOKEN" https://gitea.stump.rocks/api/v1/user`,
		"git push https://x-access-token:$GH_TOKEN@github.com/stump-wtf/harness.git",
	} {
		if got := String(in); got != in {
			t.Errorf("String(%q) = %q, want it unchanged", in, got)
		}
	}
}
