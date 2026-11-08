# Patched copy of github.com/anacrolix/torrent v1.61.0

Shark BiteTorrent uses this copy (via a `replace` in the root `go.mod`) instead of the published module, for one reason: a security fix that upstream has only in an untagged commit.

**GHSA-2wrx-84qj-4pcg** (GO-2026-6894, Dependabot alert #9): the bencode decoder recursed once per level of nested dicts and lists, with no limit. Bencode comes from `.torrent` files and from other peers (the extension handshake, metadata, the DHT), so a crafted message nested a few million levels deep could crash the app with a fatal stack overflow (Go can't recover from that), and deeply nested v2 file trees decoded quadratically. Fixed upstream in [`76452a2c8a2f`](https://github.com/anacrolix/torrent/commit/76452a2c8a2f) (2026-09-11), 128 commits past v1.61.0, with no release tagged since.

## The patch

That one commit, applied to the published v1.61.0 as it is (`git show 76452a2c8a2f`), except its `CHANGELOG.md` entry (v1.61.0 has no CHANGELOG):

- `bencode/decode.go`: `Decoder.MaxDepth` (default `DefaultMaxDepth`, 64): input nested deeper is refused with a `SyntaxError`; `MaxStrLen` is also enforced on values decoded for `Unmarshaler` fields.
- Its tests: `bencode/decode_test.go`, `bencode/fuzz_test.go`, `metainfo/recursion_test.go`.

Everything else is the published v1.61.0, unchanged (MPL-2.0, see LICENSE). `scripts/check-torrent-patch.sh` (CI runs it) checks exactly that.

## Dropping this copy

Planned: move to upstream at or after `76452a2c8a2f` (or a tagged release that includes it), remove the `replace` and this directory, and drop the CI check. That brings the 128 other upstream commits too (storage, announcing, piece hashing), so it needs the full release testing: `go test ./...`, the engine leak test, and real downloads to the NAS on each OS.
