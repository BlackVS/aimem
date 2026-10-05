// Package installps tests pieces of the Windows installer, install.ps1, that
// can run without installing anything: each is a function between marker
// comments, extracted and run in PowerShell with synthetic inputs. The
// installer itself stays one file, which boot.ps1 fetches alone.
package installps
