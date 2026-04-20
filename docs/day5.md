  # Terminal 1 — bootstrap leader                                                   
  go run ./cmd/paladin-core cluster --id 1 --http :8080 --bootstrap
  # Wait for "entering leader state" then "Self-registered 1 -> 127.0.0.1:8080"     
                                                                                    
  # Terminal 2 — follower                                                           
  go run ./cmd/paladin-core cluster --id 2 --raft 127.0.0.1:9002 --http :8081 --join
   localhost:8080                                                                   
                                                                  
  Verify forwarding:                                                                
                                                                  
  # Write through the follower — it should forward to leader and succeed.           
  curl -X PUT http://localhost:8081/api/v1/config/public/prod/db_host -d '10.0.0.1' 
                                                                                    
  # Confirm peer mapping was replicated to follower:                                
  curl http://localhost:8081/api/v1/config/__paladin/peers/1 