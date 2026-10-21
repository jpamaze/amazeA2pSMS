-- KEYS[1] deal:{id}   KEYS[2] deal:{id}:res   ARGV[1] msgID
-- Gives the message's reserved segments back. Idempotent.
local n = redis.call('HGET', KEYS[2], ARGV[1])
if not n then
  return 0
end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HINCRBY', KEYS[1], 'used', -tonumber(n))
return 1
