# foxfastdl
Tiny proxy that provides a [Source Engine FastDL](https://developer.valvesoftware.com/wiki/FastDL) HTTP server backed by one or more SSH/SFTP connections to underlying game servers. Performs no caching on its own, so make sure to run it behind a CDN.

# Usage
```
$ FASTDL_SERVER1=sftp://USER:PASS@orchid.nodes.pyro.sh:2022/tf/?name=pound3
$ FASTDL_SERVER2=sftp://USER:PASS@camellia.nodes.pyro.sh:2022/tf/?name=pound4
$ ./foxfastdl
2025/08/11 03:01:32 foxfastdl v0.1.0 (c) 2025 Lua MacDougall <lua@foxgirl.dev>
2025/08/11 03:01:32 connected pound3 at orchid.nodes.pyro.sh:2022
2025/08/11 03:01:33 connected pound4 at camellia.nodes.pyro.sh:2022
2025/08/11 03:01:33 listening on :8080
2025/08/11 03:01:39 GET pound3 tf
2025/08/11 03:01:41 GET pound3 tf/maps
2025/08/11 03:01:47 GET pound3 tf/maps/arena_granary.bsp
2025/08/11 03:01:58 GET pound3 tf/maps/arena_granary.bsp
```
![](https://dl.vixen.link/3zwf2p/Screenshot%202025-08-11%20030252.png)
![](https://dl.vixen.link/885m54/Screenshot%202025-08-11%20030259.png)
![](https://dl.vixen.link/c6bazm/Screenshot%202025-08-11%20030310.png)

# License
Made with ❤ by Lua ([foxgirl.dev](https://foxgirl.dev/)).
Licensed under [MIT](LICENSE).
