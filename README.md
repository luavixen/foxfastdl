# foxfastdl
Tiny proxy that provides a [Source Engine FastDL](https://developer.valvesoftware.com/wiki/FastDL) HTTP server backed by one or more SSH/SFTP connections to underlying game servers.

Important notes:
- Does not support Bzip2, you should use compressed BSPs.
- Performs no caching on its own, so make sure to run it behind a CDN.
- Allows you to require a `x-foxfastdl-password` which you can set from your CDN.
- Monitors server connections, attempting to reconnect automatically, and exiting on connection loss.

# Usage
You can configure foxfastdl with environment variables:
- `FASTDL_SERVERn` - list of `sftp://` URLs to proxy, supply server name with `?name=xxxxx`
- `FASTDL_PASSWORD` - required value for the `x-foxfastdl-password` header, or an empty string to disable
- `FASTDL_PATHS` - comma-seperated lists of directories to expose, default `maps,materials,models,sound,particles,scripts,resource,replay`
- `FASTDL_BIND` - listen for HTTP connections on this port, default `:8080`
- `FASTDL_MONITOR_SECONDS` - delay between checking server connections when idle, zero to disable, default `60`

Here's an example of what foxfastdl looks like:
```
$ FASTDL_SERVER1=sftp://USER:PASS@orchid.nodes.pyro.sh:2022/tf/?name=pound3
$ FASTDL_SERVER2=sftp://USER:PASS@camellia.nodes.pyro.sh:2022/tf/?name=pound4
$ ./foxfastdl
foxfastdl v0.3.0 (c) 2025 Lua MacDougall <lua@foxgirl.dev>
connected pound3 at orchid.nodes.pyro.sh:2022
connected pound4 at camellia.nodes.pyro.sh:2022
listening on :8080
GET /
GET /favicon.ico
GET /pound3
GET /pound3/maps
GET /pound3/maps/koth_harvest_final.bsp
```
![](https://dl.vixen.link/jgrka5/Screenshot%202025-08-12%20231455.png)
![](https://dl.vixen.link/reefb6/Screenshot%202025-08-12%20231501.png)
![](https://dl.vixen.link/drkasf/Screenshot%202025-08-12%20231540.png)

# License
Made with ❤ by Lua ([foxgirl.dev](https://foxgirl.dev/)).
Licensed under [MIT](LICENSE).
