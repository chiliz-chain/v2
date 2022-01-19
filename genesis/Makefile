build: .
	yarn compile && node build-abi.js && node create-genesis.js
	cp -r ./build/abi ../core/systemcontracts/abi
	cp -r ./build/abi ../backoffice/src/abi

all: build