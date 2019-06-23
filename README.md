# Goblet: Git caching proxy

Goblet is a Git proxy server that caches repositories for read access. Git
clients can configure their repositories to use this as an HTTP proxy server,
and this proxy server serves git-fetch requests if it can be served from the
local cache.

In the Git protocol, the server creates a pack-file dynamically based on the
objects that the clients have. Because of this, caching Git protocol response
is hard as different client needs a different response. Goblet parses the
content of the HTTP POST requests and tells if the request can be served from
the local cache.

This is developed to reduce the automation traffic to googlesource.com. Goblet
would be useful if you need to run a Git read-only mirroring server to offload
the traffic.

This is not an official Google product (i.e. a 20% project).

## Usage

Goblet is intended to be used as a library. You would need to write some glue
code. This repository includes the glue code for googlesource.com. See
`goblet-server` and `google` directories.

## Limitations

Note that Goblet forwards the ls-refs traffic to the upstream server. If the
upstream server is down, Goblet is effectively down. Technically, we can modify
Goblet to serve even if the upstream is down, but the current implementation
doesn't do such thing.
