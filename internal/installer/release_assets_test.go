package installer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release publishes boot.ps1 next to the binaries and lists it in
// SHA256SUMS, so a Windows install can check the script against the
// release before running it as a file.
func TestReleaseShipsBootPs1InTheChecksums(t *testing.T) {
	wf := read(t, filepath.Join(".github", "workflows", "release.yml"))
	cp := regexp.MustCompile(`(?m)^\s*cp boot\.ps1 dist/boot\.ps1\s*$`).FindStringIndex(wf)
	sums := strings.Index(wf, "sha256sum * > SHA256SUMS")
	if cp == nil {
		t.Fatal("release.yml does not copy boot.ps1 into dist/")
	}
	if sums < 0 || cp[0] > sums {
		t.Fatal("release.yml copies boot.ps1 into dist/ after SHA256SUMS is written, so the sums do not list it")
	}
}

// boot.ps1 runs install.ps1 under RemoteSigned, not Bypass. That runs the
// installer extracted from the release archive even when the archive
// itself carries the Mark of the Web, because tar does not copy the mark
// to what it extracts, and it still refuses an installer that carries the
// mark itself. The archive is built and extracted here with Windows' own
// tar, the one boot.ps1 finds on a user's PATH.
func TestBootPs1RunsTheInstallerUnderRemoteSigned(t *testing.T) {
	ps := powerShell(t)
	block := extract(t, "boot.ps1", "run-installer")
	if strings.Contains(block, "Bypass") {
		t.Fatal("boot.ps1 runs the installer with the execution policy bypassed")
	}
	stub := `param([string]$Target, [switch]$Bootstrap, [switch]$UserOnly)
Write-Output 'installer ran'
`
	mark := "Set-Content -LiteralPath $p -Stream Zone.Identifier -Value \"[ZoneTransfer]`r`nZoneId=3`r`n\"\n"
	for _, c := range []struct {
		name, prepare, want string
	}{
		{"extracted from a marked archive", `$tar = Join-Path $env:SystemRoot 'System32\tar.exe'
$src = Join-Path $env:WORK 'src'
New-Item -ItemType Directory -Force $src | Out-Null
Copy-Item -LiteralPath $env:STUB (Join-Path $src 'install.ps1')
$p = Join-Path $env:WORK 'src.tar.gz'
& $tar -czf $p -C $src install.ps1
if ($LASTEXITCODE -ne 0) { throw 'tar -c failed' }
` + mark + `& $tar -xzf $p -C $dest
if ($LASTEXITCODE -ne 0) { throw 'tar -x failed' }
`, "installer ran"},
		{"marked itself", `$p = Join-Path $dest 'install.ps1'
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
			script := "$ErrorActionPreference = 'Stop'\n$dest = $env:DEST\n" + c.prepare +
				"try {\n" + block + "} catch { Write-Output \"failure=[$_]\" }\n"
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
