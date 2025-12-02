git-pages
=========

_git-pages_ is a static site server for use with Git forges (i.e. a GitHub Pages replacement). It is written with efficiency in mind, scaling horizontally to any number of deployed sites and concurrent requests and serving sites up to hundreds of megabytes in size, while being equally suitable for single-user deployments.

It is implemented in Go and has no other mandatory dependencies, although it is designed to be used together with the [Caddy server][caddy] (for TLS termination) and an [Amazon S3](https://aws.amazon.com/s3/) compatible object store (for horizontal scalability of storage).

The included Docker container provides everything needed to deploy a Pages service, including zero-configuration on-demand provisioning of TLS certificates from [Let's Encrypt](https://letsencrypt.org/), and runs on any commodity cloud infrastructure. There is also a first-party deployment of _git-pages_ at [grebedoc.dev](https://grebedoc.dev).

[caddy]: https://caddyserver.com/


Quickstart
----------

You will need [Go](https://go.dev/) 1.25 or newer. Run:

```console
$ mkdir -p data
$ cp conf/config.example.toml config.toml
$ PAGES_INSECURE=1 go run .
```

These commands starts an HTTP server on `0.0.0.0:3000` and use the `data` directory for persistence. **Authentication is disabled via `PAGES_INSECURE=1`** to avoid the need to set up a DNS server as well; never enable `PAGES_INSECURE=1` in production.

To publish a site, run the following commands (consider also using the [git-pages-cli] tool):

```console
$ curl http://localhost:3000/ -X PUT --data https://codeberg.org/git-pages/git-pages.git
b70644b523c4aaf4efd206a588087a1d406cb047
```

The `pages` branch of the repository is now available at http://localhost:3000/!

[git-pages-cli]: https://codeberg.org/git-pages/git-pages-cli


Deployment
----------

The first-party container supports running _git-pages_ either standalone or together with [Caddy][].

To run _git-pages_ standalone and use the filesystem to store site data:

```console
$ docker run -u $(id -u):$(id -g) --mount type=bind,src=$(pwd)/data,dst=/app/data -p 3000:3000 codeberg.org/git-pages/git-pages:latest
```

To run _git-pages_ with Caddy and use an S3-compatible endpoint to store site data and TLS key material:

```console
$ docker run -e PAGES_STORAGE_TYPE -e PAGES_STORAGE_S3_ENDPOINT -e PAGES_STORAGE_S3_REGION -e PAGES_STORAGE_S3_ACCESS_KEY_ID -e PAGES_STORAGE_S3_SECRET_ACCESS_KEY -e PAGES_STORAGE_S3_BUCKET -e ACME_EMAIL -p 80:80 -p 443:443 codeberg.org/git-pages/git-pages:latest supervisord
```


Features
--------

* In response to a `GET` or `HEAD` request, the server selects an appropriate site and responds with files from it. A site is a combination of the hostname and (optionally) the project name.
    - The site is selected as follows:
        - If the URL matches `https://<hostname>/<project-name>/...` and a site was published at `<project-name>`, this project-specific site is selected.
        - If the URL matches `https://<hostname>/...` and the previous rule did not apply, the index site is selected.
    - Site URLs that have a path starting with `.git-pages/...` are reserved for _git-pages_ itself.
        - The `.git-pages/health` URL returns `ok` with the `Last-Modified:` header set to the manifest modification time.
        - The `.git-pages/manifest.json` URL returns a [ProtoJSON](https://protobuf.dev/programming-guides/json/) representation of the deployed site manifest with the `Last-Modified:` header set to the manifest modification time. It enumerates site structure, redirect rules, and errors that were not severe enough to abort publishing. Note that **the manifest JSON format is not stable and will change without notice**.
        - **With feature `archive-site`:** The `.git-pages/archive.tar` URL returns a tar archive of all site contents, including `_redirects` and `_headers` files (reconstructed from the manifest), with the `Last-Modified:` header set to the manifest modification time. Compression can be enabled using the `Accept-Encoding:` HTTP header (only).
* In response to a `PUT` or `POST` request, the server updates a site with new content. The URL of the request must be the root URL of the site that is being published.
    - If the `PUT` method receives an `application/x-www-form-urlencoded` body, it contains a repository URL to be shallowly cloned. The `Branch` header contains the branch to be checked out; the `pages` branch is used if the header is absent.
    - If the `PUT` method receives an `application/x-tar`, `application/x-tar+gzip`, `application/x-tar+zstd`, or `application/zip` body, it contains an archive to be extracted.
    - The `POST` method requires an `application/json` body containing a Forgejo/Gitea/Gogs/GitHub webhook event payload. Requests where the `ref` key contains anything other than `refs/heads/pages` are ignored, and only the `pages` branch is used. The `repository.clone_url` key contains a repository URL to be shallowly cloned.
    - If the received contents is empty, performs the same action as `DELETE`.
* In response to a `DELETE` request, the server unpublishes a site. The URL of the request must be the root URL of the site that is being unpublished. Site data remains stored for an indeterminate period of time, but becomes completely inaccessible.
* If a `Dry-Run: yes` header is provided with a `PUT`, `DELETE`, or `POST` request, only the authorization checks are run; no destructive updates are made. Note that this functionality was added in _git-pages_ v0.2.0.
* All updates to site content are atomic (subject to consistency guarantees of the storage backend). That is, there is an instantaneous moment during an update before which the server will return the old content and after which it will return the new content.
* Files with a certain name, when placed in the root of a site, have special functions:
    - [Netlify `_redirects`][_redirects] file can be used to specify HTTP redirect and rewrite rules. The _git-pages_ implementation currently does not support placeholders, query parameters, or conditions, and may differ from Netlify in other minor ways. If you find that a supported `_redirects` file feature does not work the same as on Netlify, please file an issue. (Note that _git-pages_ does not perform URL normalization; `/foo` and `/foo/` are *not* the same, unlike with Netlify.)
    - [Netlify `_headers`][_headers] file can be used to specify custom HTTP response headers (if allowlisted by configuration). In particular, this is useful to enable [CORS requests][cors]. The _git-pages_ implementation may differ from Netlify in minor ways; if you find that a `_headers` file feature does not work the same as on Netlify, please file an issue.
* Support for SHA-256 Git hashes is [limited by go-git][go-git-sha256]; once go-git implements the required features, _git-pages_ will automatically gain support for SHA-256 Git hashes. Note that shallow clones (used by _git-pages_ to conserve bandwidth if available) aren't supported yet in the Git protocol as of 2025.

[_redirects]: https://docs.netlify.com/manage/routing/redirects/overview/
[_headers]: https://docs.netlify.com/manage/routing/headers/
[cors]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CORS
[go-git-sha256]: https://github.com/go-git/go-git/issues/706


Authorization
-------------

DNS is the primary authorization method, using either TXT records or wildcard matching. In certain cases, git forge authorization is used in addition to DNS.

The authorization flow for content updates (`PUT`, `DELETE`, `POST` requests) proceeds sequentially in the following order, with the first of multiple applicable rule taking precedence:

1. **Development Mode:** If the environment variable `PAGES_INSECURE` is set to a truthful value like `1`, the request is authorized.
2. **DNS Challenge:** If the method is `PUT`, `DELETE`, `POST`, and a well-formed `Authorization:` header is provided containing a `<token>`, and a TXT record lookup at `_git-pages-challenge.<host>` returns a record whose concatenated value equals `SHA256("<host> <token>")`, the request is authorized.
    - **`Pages` scheme:** Request includes an `Authorization: Pages <token>` header.
    - **`Basic` scheme:** Request includes an `Authorization: Basic <basic>` header, where `<basic>` is equal to `Base64("Pages:<token>")`. (Useful for non-Forgejo forges.)
3. **DNS Allowlist:** If the method is `PUT` or `POST`, and the request URL is `scheme://<user>.<host>/`, and a TXT record lookup at `_git-pages-repository.<host>` returns a set of well-formed absolute URLs, and (for `PUT` requests) the body contains a repository URL, and the requested clone URLs is contained in this set of URLs, the request is authorized.
4. **Wildcard Match (content):** If the method is `POST`, and a `[[wildcard]]` configuration section exists where the suffix of a hostname (compared label-wise) is equal to `[[wildcard]].domain`, and (for `PUT` requests) the body contains a repository URL, and the requested clone URL is a *matching* clone URL, the request is authorized.
    - **Index repository:** If the request URL is `scheme://<user>.<host>/`, a *matching* clone URL is computed by templating `[[wildcard]].clone-url` with `<user>` and `<project>`, where `<project>` is computed by templating each element of `[[wildcard]].index-repos` with `<user>`, and `[[wildcard]]` is the section where the match occurred.
    - **Project repository:** If the request URL is `scheme://<user>.<host>/<project>/`, a *matching* clone URL is computed by templating `[[wildcard]].clone-url` with `<user>` and `<project>`, and `[[wildcard]]` is the section where the match occurred.
5. **Forge Authorization:** If the method is `PUT`, and the body contains an archive, and a `[[wildcard]]` configuration section exists where the suffix of a hostname (compared label-wise) is equal to `[[wildcard]].domain`, and `[[wildcard]].authorization` is non-empty, and the request includes a `Forge-Authorization:` header, and the header (when forwarded as `Authorization:`) grants push permissions to a repository at the *matching* clone URL (as defined above) as determined by an API call to the forge, the request is authorized. (This enables publishing a site for a private repository.)
5. **Default Deny:** Otherwise, the request is not authorized.

The authorization flow for metadata retrieval (`GET` requests with site paths starting with `.git-pages/`) in the following order, with the first of multiple applicable rule taking precedence:

1. **Development Mode:** Same as for content updates.
2. **DNS Challenge:** Same as for content updates.
3. **Wildcard Match (metadata):** If a `[[wildcard]]` configuration section exists where the suffix of a hostname (compared label-wise) is equal to `[[wildcard]].domain`, the request is authorized.
4. **Default Deny:** Otherwise, the request is not authorized.


Observability
-------------

_git-pages_ has robust observability features built in:
* The metrics endpoint (bound to `:3002` by default) returns Go, pages server, and storage backend metrics in the [Prometheus](https://prometheus.io/) format.
* Optional [Sentry](https://sentry.io/) integration allows greater visibility into the application. The `ENVIRONMENT` environment variable configures the deploy environment name (`development` by default).
    * If `SENTRY_DSN` environment variable is set, panics are reported to Sentry.
    * If `SENTRY_DSN` and `SENTRY_LOGS=1` environment variables are set, logs are uploaded to Sentry.
    * If `SENTRY_DSN` and `SENTRY_TRACING=1` environment variables are set, traces are uploaded to Sentry.
* Optional syslog integration allows transmitting application logs to a syslog daemon. When present, the `SYSLOG_ADDR` environment variable enables the integration, and the variable's value is used to configure the absolute path to a Unix socket (usually located at `/dev/log` on Unix systems) or a network address of one of the following formats:
    * for TLS over TCP: `tcp+tls://host:port`;
    * for plain TCP: `tcp://host:post`;
    * for UDP: `udp://host:port`.


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

*This was the original architecture and it is no longer used. Migration to v2 was last available in commit [7e9cd17b](https://codeberg.org/git-pages/git-pages/commit/7e9cd17b70717bea2fe240eb6a784cb206243690).*

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

[0-clause BSD](LICENSE.txt)
