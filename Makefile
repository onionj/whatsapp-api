

# Define the output directory for the builds
OUTPUT_DIR := ./build
version := 1.0.0

# Define the name of your binary
BINARY_NAME := whatsappapi

# List of supported platforms (you can add more if needed)
PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64

# Default target: build for the current OS and architecture
build:
	go build -ldflags="-s -w -X main.version=$(version)" -o $(OUTPUT_DIR)/$(BINARY_NAME)

# Target to build for all supported platforms
buildall: $(addprefix build-,$(PLATFORMS))

# Individual targets to build for specific platforms
build-%:
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) go build -ldflags="-s -w -X main.version=$(version)" -o $(OUTPUT_DIR)/$(BINARY_NAME)-$(word 1,$(subst -, ,$*))-$(word 2,$(subst -, ,$*))$(if $(findstring windows,$(word 1,$(subst -, ,$*))),.exe)

# Target to generate checksums for the release files
buildall-get-checksums: clean buildall
	(cd $(OUTPUT_DIR) && shasum -a 256 * > $(BINARY_NAME)-checksums.sha256)

# Target to clean the build directory
clean:
	rm -rf $(OUTPUT_DIR)

test:
	go clean -testcache && go test ./... -v
