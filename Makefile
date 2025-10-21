tag=latest

all: server

server: dummy
	buildtool-model ./ 
	buildtool-router ./ > ./router/router.go
	go build -o bin/ninedragons_back main.go

fswatch:
	fswatch -0 controllers | xargs -0 -n1 build/notify.sh

run:
	gin --port 8002 -a 8002 --bin bin/ninedragons_back run main.go

allrun:
	fswatch -0 controllers | xargs -0 -n1 build/notify.sh &
	gin --port 8002 -a 8002 --bin bin/ninedragons_back run main.go

test: dummy
	go test -v ./...

linux:
	env GOOS=linux GOARCH=amd64 go build -o bin/ninedragons_back.linux main.go

dockerbuild:
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags '-s' -o bin/ninedragons_back.linux main.go

docker: dockerbuild
	docker build --platform linux/amd64 -t kobums/ninedragons_back:$(tag) .

dockerrun:
	docker run --env-file .env --platform linux/amd64 -d --name="ninedragons_back" -p 8002:8002 kobums/ninedragons_back:$(tag)

push: docker
	docker push kobums/ninedragons_back:$(tag)

clean:
	rm -f bin/ninedragons_back

dummy: