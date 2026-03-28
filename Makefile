# Nenya AI Gateway Makefile
BINARY = nenya
CONFIG = config.toml
EXAMPLE_CONFIG = example.config.toml
SERVICE = nenya.service
SECRETS_DOC = SECRETS_FORMAT.md

.PHONY: all build install clean test lint vet

all: build

build:
	go build -o $(BINARY) ./main.go

install: build
	install -Dm755 $(BINARY) /usr/local/bin/$(BINARY)
	install -Dm644 $(EXAMPLE_CONFIG) /etc/nenya/$(CONFIG)
	install -Dm644 $(SERVICE) /etc/systemd/system/$(SERVICE)
	systemctl daemon-reload
	@echo "Installed. Configure secrets as per $(SECRETS_DOC) and enable service:"
	@echo "  systemctl enable --now $(SERVICE)"

clean:
	rm -f $(BINARY)

test:
	go test ./...

lint:
	go fmt ./...
	go vet ./...

vet:
	go vet ./...

run: build
	@mkdir -p creds
	@if [ ! -f creds/secrets ]; then \
		echo "WARNING: Creating dummy secrets file for local testing. Replace with real API keys!"; \
		echo '{' > creds/secrets; \
		echo '  "client_token": "test-client-token",' >> creds/secrets; \
		echo '  "gemini_key": "dummy-gemini-key",' >> creds/secrets; \
		echo '  "deepseek_key": "dummy-deepseek-key",' >> creds/secrets; \
		echo '  "zai_key": "dummy-zai-key"' >> creds/secrets; \
		echo '}' >> creds/secrets; \
	fi
	CREDENTIALS_DIRECTORY=./creds ./$(BINARY) -config $(CONFIG)

.PHONY: release
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BINARY)-linux-amd64 ./main.go
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BINARY)-linux-arm64 ./main.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o $(BINARY)-darwin-amd64 ./main.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o $(BINARY)-darwin-arm64 ./main.go