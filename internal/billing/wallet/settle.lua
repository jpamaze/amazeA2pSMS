-- KEYS[1] wallet hash, KEYS[2] holds hash
-- ARGV[1] hold_id, ARGV[2] actual charge
-- Returns {1, new_balance} on success, {0, 0} for an unknown hold.
local amount = redis.call('HGET', KEYS[2], ARGV[1])
if not amount then
  return {0, 0}
end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HINCRBY', KEYS[1], 'held', -tonumber(amount))
local balance = redis.call('HINCRBY', KEYS[1], 'balance', -tonumber(ARGV[2]))
return {1, balance}
