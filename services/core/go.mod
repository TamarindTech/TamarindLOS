module github.com/TamarindTech/TamarindLOS/services/core

go 1.24

require (
	github.com/cedar-policy/cedar-go v1.8.0
	github.com/google/cel-go v0.30.0
	github.com/jackc/pgx/v5 v5.7.6
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/text v0.24.0
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	// Pinned: newer go-internal releases require Go 1.25; pulled in only by dependencies' tests.
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/sync v0.13.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)
