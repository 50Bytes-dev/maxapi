.PHONY: build run test clean

# static/api/spec.yml is written by hand; TestSpecMatchesRoutes checks that
# it lists exactly the routes of routes.go.

build:
	go build -o maxapi .

run:
	go run .

test:
	go build ./... && go vet ./... && go test ./...

clean:
	rm -f maxapi
