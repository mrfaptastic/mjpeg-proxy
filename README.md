# mjpeg-proxy
Republish a MJPEG HTTP image stream using a server in Go

### Running it (example):
Bind the proxy to port 20000
Proxy the feed coming from "http://xxx.xxx.xxx.xxx/mjpg"

```
user@random:~/mjpeg-proxy# go run mjpeg-proxy.go chunker.go digest.go pubsub.go  -bind ":20000" -source "http://xxx.xxx.xxx.xxx/mjpg"
```
