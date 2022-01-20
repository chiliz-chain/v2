build: *
	yarn compile && node build-abi.js
	go run ./create-genesis.go
	cp -r ./build/abi ../core/systemcontracts/abi
	cp -r ./build/abi ../backoffice/src/abi

all: build
