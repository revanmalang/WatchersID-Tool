export CGO_ENABLED = 0
export NEXT_TELEMETRY_DISABLED = 1

.PHONY: build
build: build-admin
	go build ./cmd/watchersid

.PHONY: build-admin
build-admin:
	cd admin && \
	yarn install --frozen-lockfile && \
	yarn run export && \
    mv dist ../cmd/watchersid/admin

.PHONY: clean
clean:
	rm -f watchersid
	rm -rf ./cmd/watchersid/admin
	rm -rf ./admin/dist
	rm -rf ./admin/.next