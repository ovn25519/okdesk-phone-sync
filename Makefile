BINARY := okdesk-phone-sync
CMD := ./cmd/okdesk-phone-sync
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test vet fmt run once clean

# Сборка статически слинкованного бинарника.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

# Модульные тесты.
test:
	go test ./...

# Статический анализ.
vet:
	go vet ./...

# Форматирование.
fmt:
	gofmt -w .

# Запуск демона.
run: build
	./$(BINARY) -config config.toml

# Один проход и завершение (удобно для проверки конфигурации).
once: build
	./$(BINARY) -config config.toml -once

clean:
	rm -f $(BINARY)
