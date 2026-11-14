package installer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release publishes every file an installer reads next to the
// binaries and lists it in SHA256SUMS: the boot scripts check what they run
// against those sums, and an aiforge export is these assets as published.
func TestReleaseShipsTheInstallerFilesInTheChecksums(t *testing.T) {
	wf := read(t, filepath.Join(".github", "workflows", "release.yml"))
	sums := strings.Index(wf, "sha256sum * > SHA256SUMS")
	if sums < 0 {
		t.Fatal("release.yml writes no SHA256SUMS")
	}
	for src, asset := range map[string]string{
		"boot.sh": "boot.sh", "boot.ps1": "boot.ps1", "install.sh": "install.sh", "install.ps1": "install.ps1",
		"install-hub.sh": "install-hub.sh", ".opencode/plugin/aimem.ts": "aimem-opencode.ts",
	} {
		cp := regexp.MustCompile(`(?m)^\s*cp ` + regexp.QuoteMeta(src) + ` dist/` + regexp.QuoteMeta(asset) + `\s*$`).FindStringIndex(wf)
		if cp == nil {
			t.Errorf("release.yml does not copy %s into dist/ as %s", src, asset)
		} else if cp[0] > sums {
			t.Errorf("release.yml copies %s into dist/ after SHA256SUMS is written, so the sums do not list it", src)
		}
	}
}

// boot.ps1 runs install.ps1 under RemoteSigned, not Bypass. The block
// clears the Mark of the Web from the installer, which boot.ps1 has checked
// against SHA256SUMS by then (a copy from a downloaded export carries the
// mark), so the installer runs; without that step RemoteSigned refuses it.
func TestBootPs1RunsTheInstallerUnderRemoteSigned(t *testing.T) {
	ps := powerShell(t)
	block := extract(t, "boot.ps1", "run-installer")
	if strings.Contains(block, "Bypass") {
		t.Fatal("boot.ps1 runs the installer with the execution policy bypassed")
	}
	const unblock = "  Unblock-File -LiteralPath $installer\n"
	if !strings.Contains(block, unblock) {
		t.Fatal("boot.ps1's run-installer block does not unblock the installer")
	}
	stub := `param([string]$Target, [switch]$Bootstrap, [switch]$UserOnly)
Write-Output 'installer ran'
`
	mark := "Set-Content -LiteralPath $p -Stream Zone.Identifier -Value \"[ZoneTransfer]`r`nZoneId=3`r`n\"\n"
	for _, c := range []struct {
		name, prepare, want string
	}{
		{"marked, then unblocked by the block", `$p = Join-Path $dest 'install.ps1'
Copy-Item -LiteralPath $env:STUB $p
` + mark, "installer ran"},
		{"marked, and the block's unblock removed", `$p = Join-Path $dest 'install.ps1'
Copy-Item -LiteralPath $env:STUB $p
` + mark, "is not digitally signed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "dest")
			if err := os.MkdirAll(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			stubFile := filepath.Join(dir, "stub.ps1")
			if err := os.WriteFile(stubFile, []byte(stub), 0o600); err != nil {
				t.Fatal(err)
			}
			b := block
			if strings.Contains(c.name, "removed") {
				b = strings.Replace(b, unblock, "", 1)
			}
			script := "$ErrorActionPreference = 'Stop'\n$dest = $env:DEST\n" + c.prepare +
				"try {\n" + b + "} catch { Write-Output \"failure=[$_]\" }\n"
			file := filepath.Join(dir, "run.ps1")
			if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runPowerShell(t, ps, dir, append(os.Environ(), "DEST="+dest, "WORK="+dir, "STUB="+stubFile), file)
			if err != nil || !strings.Contains(out, c.want) {
				t.Fatalf("want %q (%v):\n%s", c.want, err, out)
			}
		})
	}
}
