go run ./cmd/paladin-core cluster --id 1 --http :8080 --bootstrap  

$ curl "http://localhost:8080/api/v1/watch/public/prod/?revision=1&timeout=30"

$ curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'