# This Makefile is meant to be used by people that do not usually work
# with Go source code. If you know what GOPATH is then you probably
# don't need to bother with make.

.PHONY: geth android ios evm all test truffle-test clean
.PHONY: docker

GOBIN = ./build/bin
GO ?= latest
GORUN = go run
GIT_COMMIT=$(shell git rev-parse HEAD)
GIT_COMMIT_DATE=$(shell git log -n1 --pretty='format:%cd' --date=format:'%Y%m%d')

export AWS_ACCOUNT	= 475320849898.dkr.ecr.eu-west-3.amazonaws.com
export IMAGE_NAME	= blockchain/ccv2-geth
export IMAGE_TAG	=


#? geth: Build geth
geth:
	$(GORUN) build/ci.go install ./cmd/geth
	@echo "Done building."
	@echo "Run \"$(GOBIN)/geth\" to launch geth."

#? all: Build all packages and executables
all:
	$(GORUN) build/ci.go install

#? test: Run the tests
test: all
	$(GORUN) build/ci.go test -timeout 1h

truffle-test:
	docker build . -f ./docker/Dockerfile --target bsc-genesis -t bsc-genesis
	docker build . -f ./docker/Dockerfile --target bsc -t bsc
	docker build . -f ./docker/Dockerfile.truffle -t truffle-test
	docker compose -f ./tests/truffle/docker-compose.yml up genesis
	docker compose -f ./tests/truffle/docker-compose.yml up -d bsc-rpc bsc-validator1
	sleep 30
	docker compose -f ./tests/truffle/docker-compose.yml up --exit-code-from truffle-test truffle-test
	docker compose -f ./tests/truffle/docker-compose.yml down

#? lint: Run certain pre-selected linters
lint: ## Run linters.
	$(GORUN) build/ci.go lint

#? clean: Clean go cache, built executables, and the auto generated folder
clean:
	go clean -cache
	rm -fr build/_workspace/pkg/ $(GOBIN)/*

# The devtools target installs tools required for 'go generate'.
# You need to put $GOBIN (or $GOPATH/bin) in your PATH to use 'go generate'.

#? devtools: Install recommended developer tools
devtools:
	env GOBIN= go install golang.org/x/tools/cmd/stringer@latest
	env GOBIN= go install github.com/fjl/gencodec@latest
	env GOBIN= go install github.com/golang/protobuf/protoc-gen-go@latest
	env GOBIN= go install ./cmd/abigen
	@type "solc" 2> /dev/null || echo 'Please install solc'
	@type "protoc" 2> /dev/null || echo 'Please install protoc'

#? help: Build docker image
docker:
	docker build --pull -t chilizchain/ccv2-geth:latest -f Dockerfile .
docker.login:
	aws ecr get-login-password --profile=chiliz-integration --region=eu-west-3 | docker login --username AWS --password-stdin $(AWS_ACCOUNT)


docker.build_images:
	@if [ -z "$(IMAGE_TAG)" ]; \
	then echo "Please define IMAGE_TAG"; exit 1; \
	fi
	DOCKER_BUILDKIT=1 docker build --build-arg BUILDKIT_INLINE_CACHE=1 -f Dockerfile --progress plain \
	    --build-arg COMPOSER_AUTH='${COMPOSER_AUTH}' \
		--tag $(AWS_ACCOUNT)/$(IMAGE_NAME):$(IMAGE_TAG) \
		.
	docker push $(AWS_ACCOUNT)/$(IMAGE_NAME):$(IMAGE_TAG)
