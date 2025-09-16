git-pages
=========

This is a simple Go service implemented as a strawman proposal of how https://codeberg.page could work.


Features
--------

* In response to a `PUT` or `POST` request, performs a shallow in-memory clone of a git repository, checks out a tree to the filesystem, and atomically updates the version of content being served.
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

DNS is used for authorization of content updates.

- If a `[wildcard]` configuration section is specified, and if the suffix of a hostname in a `POST` request is equal to `[wildcard].domain`, then the request is authorized when and only when the repository URL in the event body matches the repository URL computed from the configuration file. Otherwise the next rule is used.

- If a `PUT` or `POST` request is received at `<hostname>` with an `Authorization: Pages <token>` header (or, in absence of such, with an `Authorization: Basic <basic>` header, where `<basic>` is equal to `Base64("Pages <token>")`), then the request is authorized when any of the the TXT records at `_git-pages-challenge.<hostname>` are equal to `SHA256("<hostname> <token>")`.


Architecture
------------

Filesystem is used as the sole mechanism for state storage.

- The `data/tree/` directory contains working trees organized by commit hash (indiscriminately of the repository they belong to). Repositories themselves are never stored on disk; they are cloned in-memory and discarded immediately after their contents is extracted.
    - The presence of a working tree directory under the appropriate commit hash is considered an indicator of its completeness. Checkouts are first done into a temporary directory and then atomically moved into place.
    - Currently a working tree is never removed, but a practical system would need to have a way to discard orphaned ones.
- The `data/www/` directory contains symlinks to working trees organized by domain and project name (e.g. `data/www/example.org/myproject`, or `data/www/example.org/.index`).
    - The presence of a symlink at the appropriate location is considered an indicator of completeness as well. Updating to a new content version is done by creating a new symlink at a temporary location and then atomically moving it into place.
    - This structure is simple enough that it may be served by e.g. Nginx instead of the Go application.
- `openat2(RESOLVE_IN_ROOT)` is used to confine GET requests strictly under the `data/` directory.

This approach has the benefits of being easy to explore and debug, but places a lot of faith onto the filesystem implementation; partial data loss, write reordering, or incomplete journalling *will* result in confusing and persistent caching issues. This is probably fine, but needs to be understood.

The specific arrangement used is clearly not optimal; at a minimum it is likely worth it to deduplicate files under `data/tree/` using hardlinks, or perhaps to put objects in a flat, content-addressed store with `data/www/` linking to each individual file. The key practical constraint will likely be the need to attribute excessively large trees to repositories they were built from (and to perform GC), which suggests adding structure and not removing it.


License
-------

[0-clause BSD](LICENSE-0BSD.txt)
