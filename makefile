GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOCLEAN=$(GOCMD) clean

build:
	$(GOBUILD) -o ratelimiter .

test:
	@echo "\n\n\nRunning test suite\n------------------------\n\n"
	$(GOTEST) -v -coverprofile=coverage.out ./...

clean:
	$(GOCLEAN)
	rm -f ratelimiter

.PHONY:
wipe-cache:
	$(GOCLEAN) -testcache -v ./...
	
.PHONY:
run-dev:
	@echo "Running docker"
	@docker-compose up -d

.PHONY:
destroy-dev:
	@echo "Destroying dev"
	@docker-compose down -v