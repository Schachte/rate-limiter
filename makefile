GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOCLEAN=$(GOCMD) clean

build:
	$(GOBUILD) -o ratelimiter .

test:
	@echo "\n\n\nRunning test suite\n------------------------\n\n"
	$(GOTEST) -v ./...

clean:
	$(GOCLEAN)
	rm -f ratelimiter

.PHONY:
wipe-cache:
	$(GOCLEAN) -testcache -v ./...
	