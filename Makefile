APP=csp-web

.PHONY: all tidy build run clean

all: build

tidy:
	go mod tidy

build:
	go build -o $(APP)

run:
	go run .

clean:
	rm -f $(APP)
