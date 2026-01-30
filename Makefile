VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/ralph .

install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" .

clean:
	rm -rf dist/

readme: build
	@echo "# ralph" > README.md
	@echo "" >> README.md
	@echo "Automated task execution loop with Claude." >> README.md
	@echo "" >> README.md
	@echo "## Usage" >> README.md
	@echo "" >> README.md
	@echo '```' >> README.md
	@./dist/ralph help >> README.md
	@echo '```' >> README.md
	@echo "" >> README.md
	@echo "## Commands" >> README.md
	@echo "" >> README.md
	@echo "### start" >> README.md
	@echo "" >> README.md
	@echo '```' >> README.md
	@./dist/ralph start --help >> README.md
	@echo '```' >> README.md
	@echo "" >> README.md
	@echo "### init" >> README.md
	@echo "" >> README.md
	@echo '```' >> README.md
	@./dist/ralph init --help >> README.md
	@echo '```' >> README.md
	@echo "" >> README.md
	@echo "### version" >> README.md
	@echo "" >> README.md
	@echo '```' >> README.md
	@echo "ralph version - Show version information" >> README.md
	@echo '```' >> README.md

.PHONY: build clean readme
