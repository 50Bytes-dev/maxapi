.PHONY: swagger build run clean

swagger:
	@go tool swag init -g main.go --outputTypes yaml -o .
	@mv swagger.yaml static/api/spec.yml

build:
	go build -o maxapi .

run:
	go run .

clean:
	rm -f maxapi

all: swagger build
