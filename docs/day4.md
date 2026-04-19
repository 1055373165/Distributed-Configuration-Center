go run ./cmd/paladin-core cluster --id 1 --http :8080 --bootstrap  

```
$ go run ./cmd/paladin-core cluster --id 1 --http :8080 --bootstrap  
2026-04-20T00:09:20.141+0800 [INFO]  raft: initial configuration: index=0 servers=[]
2026-04-20T00:09:20.141+0800 [INFO]  raft: entering follower state: follower="Node at 127.0.0.1:9001 [Follower]" leader-address= leader-id=
2026/04/20 00:09:20 PaladinCore [raft] node=1 raft=127.0.0.1:9001 http=:8080 bootstrap=true
2026-04-20T00:09:21.178+0800 [WARN]  raft: heartbeat timeout reached, starting election: last-leader-addr= last-leader-id=
2026-04-20T00:09:21.178+0800 [INFO]  raft: entering candidate state: node="Node at 127.0.0.1:9001 [Candidate]" term=2
2026-04-20T00:09:21.178+0800 [DEBUG] raft: pre-voting for self: term=2 id=1
2026-04-20T00:09:21.178+0800 [DEBUG] raft: calculated votes needed: needed=1 term=2
2026-04-20T00:09:21.178+0800 [DEBUG] raft: pre-vote received: from=1 term=2 tally=0
2026-04-20T00:09:21.178+0800 [DEBUG] raft: pre-vote granted: from=1 term=2 tally=1
2026-04-20T00:09:21.178+0800 [INFO]  raft: pre-vote successful, starting election: term=2 tally=1 refused=0 votesNeeded=1
2026-04-20T00:09:21.188+0800 [DEBUG] raft: voting for self: term=2 id=1
2026-04-20T00:09:21.210+0800 [DEBUG] raft: vote granted: from=1 term=2 tally=1
2026-04-20T00:09:21.210+0800 [INFO]  raft: election won: term=2 tally=1
2026-04-20T00:09:21.210+0800 [INFO]  raft: entering leader state: leader="Node at 127.0.0.1:9001 [Leader]"
```

$ curl "http://localhost:8080/api/v1/watch/public/prod/?revision=1&timeout=30"

$ curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'
$ curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'