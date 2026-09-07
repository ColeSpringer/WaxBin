# Upstream requests

The standing list of things WaxBin wants from the sibling Wax repos it depends
on. Only wax-series dependencies belong here, and today that is WaxLabel (the
tag reader and writer behind `meta`) and WaxFlow (the decoder and meter behind
`decode` and `analyze`), the two in go.mod; WaxTap and WaxSeal are not
dependencies (an embedding app injects the youtube provider), and what WaxDeck
wants from WaxBin lives in WaxDeck's own docs. Every entry is a candidate for
whenever upstream work is next scheduled; nothing here implies timing, and none
of it is a WaxBin prerequisite, since each entry names the workaround WaxBin
ships today and, where one exists, the test that will notice the fix landing.
Agents: when you defer something because it needs upstream support, add it here
in the same change, and put the WaxBin-side follow-up in
[deferred-work.md](deferred-work.md); when upstream lands it, do the follow-up
and remove both entries.

## WaxLabel

- **The MP4 sample entry's rate is reported verbatim for hi-res ALAC and
  FLAC.** The AudioSampleEntry's 16.16 samplerate field cannot hold a rate past
  65535 Hz, so a hi-res file's entry never carries the true one: an ALAC that
  WaxFlow wrote says 65535 and one that ffmpeg wrote says 0 (the magic cookie
  carries the real rate either way), and a FLAC-in-MP4 carries the greatest
  whole halving that fits (48000 for 96 kHz), which the FLAC-in-ISOBMFF spec
  says a reader must override from the STREAMINFO in the `dfLa` box. waxlabel
  v1.6.2 reports the field as it finds it, and `meta` copies that into the file
  row for every container WaxLabel parses (the decoder probe fills in only for
  one it cannot). So a 96 kHz ALAC catalogs at 65535 or 0 and a 96 kHz
  FLAC-in-MP4 at 48000, `upgrade` ranks alternatives by sample rate ahead of
  bit depth and bitrate, and `pidpath` converts a cue-split rip's track offsets
  as frames times that rate (a rate of 0 fails the locate outright). Wanted:
  `SampleRate` from the ALAC cookie, the way the bit depth already comes from
  it, and from the `dfLa` STREAMINFO for a `fLaC` entry. Shipped workaround:
  none; the catalog stores what the parse reports, and `scan --force` refreshes
  the rows once a fix lands. WaxFlow carries the same ask in its own
  docs/upstream-requests.md and pins the present behaviour in its oracletest
  (`TestWaxlabelReadsTheSampleEntryRateVerbatim`); WaxBin has no pin of its
  own, so the day that test fails is the cue to retire this entry too.

- **The Opus header's output gain has no write path.** The ReplayGain
  write-back gives an Opus file `R128_TRACK_GAIN` and `R128_ALBUM_GAIN` (Q7.8
  against the -23 LUFS reference) and stops there: WaxLabel treats
  `output_gain` as essence configuration rather than as a tag and offers no
  edit that sets it, so the gain a player applies by default, before it reads
  any tag, stays whatever the encoder left, and only a player that also honours
  the R128 tags sees WaxBin's measurement. `r128Gain` in tagwrite.go names the
  gap. Wanted: an edit that patches `output_gain` in the identification header
  (a two-byte field in the first packet, so only that page's checksum changes;
  the field sits ahead of the audio pages, so the essence is untouched).
  Shipped workaround: the tags alone; the header is never written.

## WaxFlow

- **WMA Pro, Lossless, and Voice do not decode.** `codec/wma` covers v1 and
  v2, and `container/asf` names the other three so a refusal says what it
  skipped. WaxLabel parses all five the same way, so those files catalog
  normally and then sit in the analyze pass's retry set: the open fails and
  WaxBin classifies it as `decode.ErrUnsupported`, the file is counted as
  skipped on every run, and it never gets a fingerprint, a loudness row, or a
  waveform, so `upgrade` cannot see it as an alternative of anything either.
  The README carries the caveat, and the `scan.excludedExts` comment says why
  `.wma` stays scanned despite it. Wanted: decoders for the three, Lossless and
  Pro first (a Windows Media Player rip made at its higher settings is one of
  those); Voice is rare in a music library. Shipped workaround: the retry set
  and the caveat sentence. If the decoders land under new codec IDs,
  `decode.TestCoverageDecodesEveryCodec` demands a fixture for each (ffmpeg has
  to make it, since WaxFlow does not encode WMA); if they land under the
  existing ID nothing in WaxBin fails, and the README sentence is the thing to
  update.

- **The cue sheet package is internal.** `waxflow/internal/cue` parses a
  sheet, bounds an MM:SS:FF the way a wire value has to be bounded, and
  converts CD frames to samples, and its doc comment says the public move is
  one WaxFlow can make later. WaxBin needs the same three things on its side of
  the boundary (a sidecar `.cue` becomes book chapters and carves a single-file
  rip into virtual tracks, and `pidpath` turns those frame offsets into the
  sample bounds it hands the transcoder), so `meta/cue.go` parses sheets
  itself, `parseCueTime` mirrors the same three bounds, and the
  frames-to-samples formula lives in both repos with a comment on each side
  saying the two must agree and cannot share a test. Wanted: the package
  public, at least `Parse`, `ParseTime`, and `Samples`, so WaxBin's parser
  becomes a thin adapter and the formula has one owner. Shipped workaround: the
  duplicate, kept in step by hand on every WaxFlow bump.
