module github.com/hashicorp/go-memdb/bench

go 1.25.0

replace github.com/hashicorp/go-memdb => ../

require (
	github.com/cilium/statedb v0.8.5-0.20260826112337-3fca4cd3c264
	github.com/hashicorp/go-memdb v0.0.0
)

require (
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
)
