git-pages
=========

_git-pages_ is a static site server for use with Git forges (i.e. a GitHub Pages replacement). It is written with efficiency in mind, scaling horizontally to any number of deployed sites and concurrent requests and serving sites up to hundreds of megabytes in size, while being equally suitable for single-user deployments.

It is implemented in Go and has no other mandatory dependencies, although it is designed to be used together with the [Caddy server](https://caddyserver.com/) (for TLS termination) and an [Amazon S3](https://aws.amazon.com/s3/) compatible object store (for horizontal scalability of storage).

The included Docker container provides everything needed to deploy a Pages service, including zero-configuration on-demand provisioning of TLS certificates from [Let's Encrypt](https://letsencrypt.org/), and runs on any commodity cloud infrastructure. There is also a first-party deployment of _git-pages_ at [grebedoc.dev](https://grebedoc.dev).


Quickstart
----------

You will need [Go](https://go.dev/) 1.25 or newer. Run:

```console
$ mkdir -p data
$ cp config.toml.example config.toml
$ INSECURE=very go run ./src
```

These commands starts an HTTP server on `0.0.0.0:3000` and use the `data` directory for persistence. **Authentication is disabled via `INSECURE=very`** to avoid the need to set up a DNS server as well; never set `INSECURE=very` in production.

To publish a site, run the following commands:

```console
$ curl http://localhost:3000/ -X PUT --data https://codeberg.org/whitequark/git-pages.git
b70644b523c4aaf4efd206a588087a1d406cb047
```

The `pages` branch of the repository is now available at http://localhost:3000/!

To get inspiration for deployment strategies, take a look at the included [Dockerfile](Dockerfile) or the [configuration](fly.toml) for [Fly.io](https://fly.io).


Features
--------

* In response to a `GET` or `HEAD` request, the server selects an appropriate site and responds with files from it. A site is a combination of the hostname and (optionally) the project name. The site is selected as follows:
    - If the URL matches `https://<hostname>/<project-name>/...` and a site was published at `<project-name>`, this project-specific site is selected.
    - If the URL matches `https://<hostname>/...` and the previous rule did not apply, the index site is selected.
* In response to a `PUT` or `POST` request, the server performs a shallow clone of the indicated git repository into a temporary location, checks out the `HEAD` commit, and atomically updates a site. The URL of the request must be the root URL of the site that is being published.
    - The `PUT` method requires an `application/x-www-form-urlencoded` body. The body contains the repository URL to be cloned.
    - The `POST` method requires an `application/json` body containing a Forgejo/Gitea/Gogs/GitHub webhook event payload. Requests where the `ref` key contains anything other than `refs/heads/pages` are ignored. The `repository.clone_url` key contains the repository URL to be cloned.
* In response to a `DELETE` request, the server unpublishes a site. The URL of the request must be the root URL of the site that is being unpublished. Site data remains stored for an indeterminate period of time, but becomes completely inaccessible.
* All updates to site content are atomic (subject to consistency guarantees of the storage backend). That is, there is an instantaneous moment during an update before which the server will return the old content and after which it will return the new content.


Authorization
-------------

DNS is used for authorization of content updates, either via TXT records or by pattern matching. The authorization flow proceeds sequentially in the following order, with the first of multiple applicable rule taking precedence:

1. **Development Mode:** If the environment variable `INSECURE` is set to the value `very`, the request is authorized to update from any clone URL.
2. **DNS Challenge:** If the method is `PUT`, `DELETE`, or `POST`, and a well-formed `Authorization:` header is provided containing a `<token>`, and a TXT record lookup at `_git-pages-challenge.<host>` returns a record whose concatenated value equals `SHA256("<host> <token>")`, the request is authorized to update from any clone URL.
    - **<code>Pages</code> scheme:** Request includes an `Authorization: Pages <token>` header.
    - **<code>Basic</code> scheme:** Request includes an `Authorization: Basic <basic>` header, where `<basic>` is equal to `Base64("Pages:<token>")`. (Useful for non-Forgejo forges.)
3. **DNS Allowlist:** If the method is `PUT` or `POST`, and a TXT record lookup at `_git-pages-repository.<host>` returns a set of well-formed absolute URLs, the request is authorized to update from clone URLs in the set.
4. **Wildcard Match:** If the method is `POST`, and a `[wildcard]` configuration section is present, and the suffix of a hostname (compared label-wise) is equal to `[wildcard].domain`, the request is authorized to update from a *matching* clone URL.
    - **Index repository:** If the request URL is `scheme://<user>.<host>/`, a *matching* clone URL is computed by templating `[wildcard.clone-url]` with `<user>` and `<project>`, where `<project>` is computed by templating each element of `[wildcard].index-repos` with `<user>`.
    - **Project repository:** If the request URL is `scheme://<user>.<host>/<project>/`, a *matching* clone URL is computed by templating `[wildcard.clone-url]` with `<user>` and `<project>`.
5. **Default Deny:** Otherwise, the request is not authorized.


Architecture (v2)
-----------------

An object store (filesystem, S3, ...) is used as the sole mechanism for state storage. The object store is expected to provide atomic operations and where necessary the backend adapter ensures as such.

- Repositories themselves never reach the object store; they are cloned to an ephemeral location and discarded immediately after their contents is extracted.
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
