git-pages
=========

This is a simple Go service implemented as a strawman proposal of how https://codeberg.page could work.


Features
--------

* In response to a `PUT` or `POST` request, performs a shallow in-memory clone of a git repository, checks out a tree to the storage backend, and atomically updates the version of content being served.
    - `PUT` method is a custom REST endpoint, `POST` method is a Forgejo webhook endpoint.
* In response to a `GET` or `HEAD` request, selects an appropriate tree and serves files from it. Supported URL patterns:
    - `https://domain.tld/project/` (routed to project-specific tree)
    - `https://domain.tld/` (routed to domain-specific tree by exclusion)


Usage
-----

You will need [Go](https://go.dev/) 1.24 or newer. Run:

```console
$ mkdir -p data
$ cp config.toml.example config.toml
$ go run ./src
```

This starts an HTTP server on `0.0.0.0:3333` whose behavior is fully determined by the `data` directory. It will accept requests to any virtual host, but must first be provisioned. For example:

```console
$ curl -v http://127.0.0.1:3333/ -X PUT -H 'Host: codeberg.page' --data https://codeberg.org/Codeberg/pages-server
*   Trying 127.0.0.1:3333...
* [snip]
< HTTP/1.1 201 Created
< Content-Location: /
< Date: Fri, 05 Sep 2025 07:19:34 GMT
< Content-Length: 41
< Content-Type: text/plain; charset=utf-8
<
915c874f8029dcb2056237440116e170de0b9489
* Connection #0 to host 127.0.0.1 left intact
```

The server will now respond to requests for this host:

```console
$ curl http://127.0.0.1:3333/ -H 'Host: codeberg.page'
<!DOCTYPE html>
[snip]
```


Authorization
-------------

DNS is used for authorization of content updates, either via TXT records or by pattern matching. The authorization flow proceeds sequentially in the following order, with the first of multiple applicable rule taking precedence:

1. **Development Mode:** If the environment variable `INSECURE` is set to the value `very`, the request is authorized to update from any clone URL.
2. **DNS Challenge:** If the method is `PUT` or `POST`, and a well-formed `Authorization:` header is provided containing a `<token>`, and a TXT record lookup at `_git-pages-challenge.<hostname>` returns a record whose concatenated value equals `SHA256("<hostname> <token>")`, the request is authorized to update from any clone URL.
    - **<code>Pages</code> scheme:** Request includes an `Authorization: Pages <token>` header.
    - **<code>Basic</code> scheme:** Request includes an `Authorization: Basic <basic>` header, where `<basic>` is equal to `Base64("Pages:<token>")`. (Useful for non-Forgejo forges.)
3. **DNS Allowlist:** If the method is `PUT` or `POST`, and a TXT record lookup at `_git-pages-repository.<hostname>` returns a set of well-formed absolute URLs, the request is authorized to update from clone URLs in the set.
4. **Wildcard Match:** If the method is `POST`, and a `[wildcard]` configuration section is present, and the suffix of a hostname (compared label-wise) is equal to `[wildcard].domain`, the request is authorized to update from a *matching* clone URL.
    - **Index repository:** If the request URL is `scheme://<hostname>/`, a *matching* clone URL is computed as `sprintf([wildcard].clone-url, <hostname>, [wildcard].index-repo)`.
    - **Project repository:** If the request URL is `scheme://<hostname>/<projectName>/`, a *matching* clone URL is computed as `sprintf([wildcard].clone-url, <hostname>, <projectName>)`.
5. **Default Deny:** Otherwise, the request is not authorized.


Architecture (v2)
-----------------

An object store (filesystem, S3, ...) is used as the sole mechanism for state storage. The object store is expected to provide atomic operations and where necessary the backend adapter ensures as such.

- Repositories themselves are never stored on disk; they are cloned in-memory and discarded immediately after their contents is extracted.
- The `blob/` prefix contains file data organized by hash of their contents (indiscriminately of the repository they belong to).
    - Very small files are stored inline in the manifest.
- The `site/` prefix contains site manifests organized by domain and project name (e.g. `site/example.org/myproject` or `site/example.org/.index`).
    - The manifest is a Protobuf object containing a flat mapping of paths to entries. An entry is comprised of type (file, directory, symlink, etc) and data, which may be stored inline or refer to a blob.
    - A small amount of internal metadata within a manifest allows attributing deployments to their source and computing quotas.
- Additionally, the object store contains *staged manifests*, representing an in-progress update operation.
    - An update first creates a staged manifest, then uploads blobs, then replaces the deployed manifest with the staged one. This avoids TOCTTOU race conditions during garbage collection.
    - Stable marshalling allows addressing staged manifests by the hash of their contents.

This approach, unlike the v1 one, cannot be easily introspected with normal Unix commands, but is very friendly to S3-style object storage services, as it does not rely on operations these services cannot support (subtree rename, directory stat, symlink/readlink).

The S3 backend, intended for (relatively) high latency connections, caches both manifests and blobs in memory. Since a manifest is necessary and sufficient to return `304 Not Modified` responses for a matching `ETag`, this drastically reduces navigation latency. Blobs are content-addressed and are an obvious target for a last level cache.


Architecture (v1)
-----------------

*This was the original architecture and it is no longer used.*

Filesystem is used as the sole mechanism for state storage.

- The `data/tree/` directory contains working trees organized by commit hash (indiscriminately of the repository they belong to). Repositories themselves are never stored on disk; they are cloned in-memory and discarded immediately after their contents is extracted.
    - The presence of a working tree directory under the appropriate commit hash is considered an indicator of its completeness. Checkouts are first done into a temporary directory and then atomically moved into place.
    - Currently a working tree is never removed, but a practical system would need to have a way to discard orphaned ones.
- The `data/www/` directory contains symlinks to working trees organized by domain and project name (e.g. `data/www/example.org/myproject` or `data/www/example.org/.index`).
    - The presence of a symlink at the appropriate location is considered an indicator of completeness as well. Updating to a new content version is done by creating a new symlink at a temporary location and then atomically moving it into place.
    - This structure is simple enough that it may be served by e.g. Nginx instead of the Go application.
- `openat2(RESOLVE_IN_ROOT)` is used to confine GET requests strictly under the `data/` directory.

This approach has the benefits of being easy to explore and debug, but places a lot of faith onto the filesystem implementation; partial data loss, write reordering, or incomplete journalling *will* result in confusing and persistent caching issues. This is probably fine, but needs to be understood.

The specific arrangement used is clearly not optimal; at a minimum it is likely worth it to deduplicate files under `data/tree/` using hardlinks, or perhaps to put objects in a flat, content-addressed store with `data/www/` linking to each individual file. The key practical constraint will likely be the need to attribute excessively large trees to repositories they were built from (and to perform GC), which suggests adding structure and not removing it.


License
-------

[0-clause BSD](LICENSE-0BSD.txt)
