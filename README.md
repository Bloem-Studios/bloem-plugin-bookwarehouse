# Bloem Bookwarehouse plugin

> [!CAUTION]
> **Offered without support.** The Bloem Bookwarehouse plugin is provided as-is, with no support of any kind. Issues are turned off, questions go unanswered, and pull requests are not accepted: they are closed without review. You are free to fork it under the AGPL v3.

A plugin for [Bloem Server](https://github.com/Bloem-Studios/bloem-server) that lets a
Bloem library read ebooks (EPUB and PDF) and audiobooks straight from a Bookwarehouse
service, and that orders books people request from the same Bookwarehouse. It is for
server administrators who already run Bookwarehouse and want its books in Bloem. It
has the plugin ID `bloem.storage.bookwarehouse`; this is version 0.4.0.

Each installation serves one source, which holds either ebooks or audiobooks. To
offer both, install it twice. Bloem asks the plugin only for lists of files and the
exact bytes it needs; it never copies the whole collection. Ebooks are downloaded in
full, checked against the size and SHA-256 that Bookwarehouse lists, and kept in a
private size-limited cache before any byte is sent. Audiobook tracks are streamed
from Bookwarehouse and never cached. For details, see the
[plugin reference](docs/reference.md).

## Install

From the [Bloem plugin catalog](https://github.com/Bloem-Studios/bloem-plugins). The
catalog release is downloaded and its checksum verified by Bloem Server, so no
approval file and no restart are needed.

1. In Bloem's admin, open the storage sources. A platform administrator opens
   **Plugins**, then the **Storage** tab. An organization administrator opens
   **Storage sources** in the organization's admin pages.
2. Choose **Add source**.
3. For ebooks, choose **Install Bookwarehouse**. When the new source appears, fill in
   **Bookwarehouse server URL** and **API key** and choose **Save connection**. For
   audiobooks, choose **Audiobooks** under **Bookwarehouse audiobooks**, fill in the
   same two fields and choose **Install audiobooks**.
4. Choose **Test** on the source. It checks that the source can list its top folder.
5. Create a library and choose the source, or folders on it, as its location.

Things to know:

- If the server URL or API key is missing, the source stays not connected: it
  installs, but every list or read fails.
- The server URL is Bookwarehouse's address, or a proxy's, with any path, such as
  `https://books.example/warehouse`. Do not add `/api/v1`.
- An organization administrator may only use an `https` address on the public
  internet, on port 443, 7480, 8443, 9000 or 9443, and may not enable plain HTTP.
- The web admin connects an ebook source only while it is not yet connected. To change
  settings later, replace the configuration through the API; this works only while no
  library uses the source, so set the size limits before creating the library.
- To upgrade, choose **Upgrade** on the source and pick a newer catalog release. The
  source keeps its settings and library.
- Native storage sources are experimental in Bloem. To run a build that is not in the
  catalog, see [Installing by hand](docs/native-installation.md).

Requests: to let Bloem order requested books through this plugin, add a request
connection in Bloem's admin on the plugin's **ebooks** or **audiobooks** request
capability, bound to the matching installation, and link the library that reads from
the source. Ebook requests are ordered by ISBN through Bookwarehouse's monitoring.
Audiobook requests search for a release through Bookwarehouse, choose one and have
Bookwarehouse download and import it; Bookwarehouse needs its torrent feature on, and
the key used must have its Curator role.

## Configuration

The plugin has one settings group, `connection`. Bloem checks it against the
manifest's JSON schema, and unknown fields are rejected.

| Setting | Meaning | Default |
| --- | --- | --- |
| `content` | `ebooks` or `audiobooks`. Never change it on a source that already has books. | `ebooks` |
| `base_url` | Bookwarehouse's `https` origin, or a reverse-proxy prefix. Required. | none |
| `api_key` | Bookwarehouse API key, sent as `X-API-Key`. Ebooks need permission to read and download books; audiobooks need permission to read them. Secret. Required. | none |
| `request_api_key` | Key with the Curator role, used for audiobook requests. Empty means `api_key`. Secret. | none |
| `preferred_format` | `epub` or `pdf`: the format ebook requests ask for. | `epub` |
| `allow_insecure_http` | Allows plain `http`, for a trusted network only: the key is then sent unencrypted. | `false` |
| `source_id` | Stable name of the source: 1 to 128 ASCII letters, digits, `_` or `-`. Ebook and audiobook sources need different IDs. | `bookwarehouse`, or `bookwarehouse-audiobooks` for audiobooks |
| `source_name` | Display name, at most 256 bytes. | `Bookwarehouse`, or `Bookwarehouse audiobooks` |
| `max_book_bytes` | Largest ebook that will be downloaded and checked. Maximum 8 GiB. | 134217728 (128 MiB) |
| `max_cache_bytes` | Space the ebook cache may use; at least `max_book_bytes`. Maximum 32 GiB. | 536870912 (512 MiB) |

The two size limits apply to ebooks only. The cache lives in the server's temporary
directory, holds at most 128 books, drops the least recently used first and is
emptied when the plugin stops or its settings change. The request connection of the
ebooks capability also accepts `allow_pdf` and `preferred_format`.

Everything else, including the folder layout, change tracking, error codes and the
Bookwarehouse API calls the plugin makes, is in the [plugin reference](docs/reference.md).

## Build from source

You need Go 1.26 or newer (`go.mod` says `go 1.26.0`; `scripts/build.sh` uses
Go 1.27.4) and, for the release metadata files, Python 3.8 or newer. The plugin uses
the public [`github.com/Bloem-Studios/bloem-plugin-sdk`](https://github.com/Bloem-Studios/bloem-plugin-sdk)
(Apache-2.0).

```sh
make test         # go test ./...
make test-race    # with the race detector; needs a C compiler
make build        # builds dist/<os>-<arch>/ for this machine
make release      # linux/amd64, linux/arm64 and darwin/arm64
```

Use another toolchain with `make test GO=/path/to/go`. `make build` writes the
executable, `manifest.json`, `SHA256SUMS` and an example approval file; metadata is
produced only for the machine you build on. See [How the plugin is tested](docs/validation.md)
for what the tests cover.

## Security

Please report vulnerabilities privately through GitHub's private vulnerability
reporting: <https://github.com/Bloem-Studios/bloem-plugin-bookwarehouse/security/advisories/new>.
Do not open a public issue for a security problem.

## Licence

GNU Affero General Public License v3. See [LICENSE](LICENSE). Earlier releases were
released under a proprietary licence and stay under it.
