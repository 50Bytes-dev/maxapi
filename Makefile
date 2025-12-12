.PHONY: swagger build run clean

swagger:
	@go tool swag init -g main.go --outputTypes yaml -o . --v3.1
	@mv swagger.yaml static/api/spec.yml
	@sed -i '' 's/main\.//g' static/api/spec.yml

build:
	go build -o maxapi .

run:
	go run .

clean:
	rm -f maxapi

all: swagger build
