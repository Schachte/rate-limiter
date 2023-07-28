# Use the official Golang image as the base image
FROM golang:1.19-alpine

# Set the working directory inside the container
WORKDIR /app

# Copy the Go module files into the container
COPY go.mod go.sum ./

# Download the Go module dependencies
RUN go mod tidy

# Copy the source code files into the container
COPY . .

# Compile the Go program
RUN go build -o server /app/cli/server.go

# Expose the port on which the server will run
EXPOSE 8080

# Run the compiled server binary
CMD ["/app/server"]