.PHONY: test run clean

test:
	go test ./... -v -count=1

run:
	go run . -config config.yaml

clean:
	rm -f local-fusion-gateway
