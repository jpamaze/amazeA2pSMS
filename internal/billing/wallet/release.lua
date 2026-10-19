-- KEYS[1] wallet hash, KEYS[2] holds hash
-- ARGV[1] hold_id
-- Returns 1 when released, 0 for an unknown hold.
local amount = redis.call('HGET', KEYS[2], ARGV[1])
if not amount then
  return 0
end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HINCRBY', KEYS[1], 'held', -tonumber(amount))
return 1
