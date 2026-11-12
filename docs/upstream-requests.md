# Upstream requests

The standing list of things WaxBin wants from the sibling Wax repos it depends
on. Only wax-series dependencies belong here.

## WaxLabel

No open requests.

## WaxFlow

- **A pid lookup answer cannot name a file whose name is not UTF-8.** The host
  answers in JSON, `{"ref": "<root>/<path>"}`, and a JSON string holds no byte
  sequence that is not UTF-8: an encoder writes U+FFFD for each stray byte, and
  `source.PIDLookup` opens the reference it decoded, which names nothing. A
  reference in a request has no such limit, since a query string percent-encodes
  any byte, and WaxBin catalogs these files by their raw path. Wanted: a form of
  the answer that carries the reference's bytes, a percent-encoded `ref` or a
  base64 field beside it. Shipped workaround: the lookup host in `pidpath`'s
  examples answers 404 for such an item, and a build that links the server
  reaches the file through `pidResolver`, which opens the raw path.
  `pidpath.TestLookupHandlerCannotNameANonUTF8File` pins the 404 and that the
  request form opens the file.
