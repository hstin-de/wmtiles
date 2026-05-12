# wmtiles build orchestration.
#
# The repo contains only source. Build artifacts (testdata fixtures, viewer
# bundle, library dist, CLI binary) are produced by these targets.
#
# Usage:
#   make             produce everything needed to run the CLI ($(BIN))
#   make test        run all tests (Go + Bun)
#   make typecheck   typecheck both TS packages
#   make clean       remove all generated artifacts

BIN := wmtiles

.DEFAULT_GOAL := all

# ────── meta targets ──────

.PHONY: all test typecheck clean

all: $(BIN)

test: testdata viewer
	go test -race ./...
	bun -F wmtiles test

typecheck:
	bun run typecheck

clean:
	rm -rf wmtiles-js/dist
	rm -f wmtiles-js/.dist.stamp
	rm -rf cmd/wmtiles/web/dist
	rm -rf format/testdata
	rm -f $(BIN)

# ────── primitives ──────

# bun install runs once; .stamp lets make track freshness.
node_modules/.stamp: package.json bun.lock wmtiles-js/package.json cmd/wmtiles/web/package.json
	bun install --frozen-lockfile
	touch $@

.PHONY: deps
deps: node_modules/.stamp

# testdata is regenerated whenever cmd/gen-testdata changes.
TESTDATA_FILES := format/testdata/minimal.wmt format/testdata/extended.wmt \
                  format/testdata/compacted.wmt format/testdata/crc_corrupted.wmt \
                  format/testdata/multistep.wmt

$(TESTDATA_FILES): cmd/gen-testdata/main.go
	mkdir -p format/testdata
	go run ./cmd/gen-testdata

.PHONY: testdata
testdata: $(TESTDATA_FILES)

# Library bundle (ESM + CJS + .d.ts). The viewer imports the package entry
# point, which resolves to dist just like published consumers do.
LIB_DIST := wmtiles-js/dist/index.js wmtiles-js/dist/index.cjs wmtiles-js/dist/index.d.ts
LIB_STAMP := wmtiles-js/.dist.stamp
LIB_SRC  := $(wildcard wmtiles-js/src/*.ts)

$(LIB_STAMP): node_modules/.stamp $(LIB_SRC)
	bun -F wmtiles build
	touch $@

$(LIB_DIST): $(LIB_STAMP)

.PHONY: lib
lib: $(LIB_STAMP)

# Viewer bundle. Triggered when viewer or library source changes.
VIEWER_DIST := cmd/wmtiles/web/dist/viewer.js
VIEWER_SRC  := $(wildcard cmd/wmtiles/web/src/*.ts cmd/wmtiles/web/index.html) \
               $(wildcard wmtiles-js/src/*.ts)

$(VIEWER_DIST): node_modules/.stamp $(LIB_STAMP) $(VIEWER_SRC)
	bun -F wmtiles-viewer build

.PHONY: viewer
viewer: $(VIEWER_DIST)

# CLI binary embeds VIEWER_DIST. The `embed` build tag activates
# cmd/wmtiles/web/embed.go (the no-tag default uses embed_stub.go and
# omits the viewer, which keeps the rest of the Go module buildable
# without bun).
$(BIN): $(VIEWER_DIST) $(wildcard **/*.go)
	go build -tags embed -o $@ ./cmd/wmtiles/
