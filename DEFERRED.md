# Deferred work

Open gaps and their reasons, so a decision to defer something is recorded rather
than rediscovered. Add an entry when you knowingly leave something unfixed; delete
one when the work lands.

Everything here is work still to do. Reasoning about work deliberately not done
belongs in the doc comment beside the code it constrains, not in this file, since
that is where someone about to get it wrong will actually read it.

## Musepack stays out of the scan

WaxLabel 1.6.0 reads and writes Musepack (`.mpc`, `.mp+`), but WaxFlow has no decoder
for it, so every Musepack file would sit in the analyze pass's retry set for good (an
undecodable `.wma` profile does the same, but the common WMA generations decode). The
two extensions sit in
`scan.excludedExts` with that reason. When WaxFlow decodes the format, move them into
`audioExts` and give `decode/formats_test.go` a fixture; the extension cross-check test
holds the two lists in step.
