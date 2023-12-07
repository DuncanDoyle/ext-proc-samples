# this is the latest tag that has been pushed
# if changes are made to the service, this tag should be updated before pushing
TAG ?= 0.1.0

.PHONY: docker-local server
all: server docker-local

server:
	CGO_ENABLED=0 GOOS=linux go build -a -o _output/server

docker-local: server
	docker build -t duncandoyle/ext-proc-error-handling:$(TAG) .

docker-push:
	./publish-interactive.sh $(TAG)

run:
	docker run -p 18080:18080 duncandoyle/ext-proc-error-handling:$(TAG)

clean:
	rm -fr _output/server
