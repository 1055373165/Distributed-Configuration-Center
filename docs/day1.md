$ go run ./cmd/paladin-core put app/db_host 10.0.0.1
OK rev=1 version=1 key=app/db_host

$ go run ./cmd/paladin-core put app/db_port 3306
# OK  rev=2  version=1  key=app/db_port
OK rev=2 version=1 key=app/db_port

$ go run ./cmd/paladin-core put app/db_host 10.0.0.2
# OK  rev=3  version=2  key=app/db_host
#     prev_value=10.0.0.1  prev_rev=1
OK rev=3 version=1 key=app/db_host

$ go run ./cmd/paladin-core list app/
  app/db_host                    = 10.0.0.2              rev=3
  app/db_port                    = 3306                  rev=2