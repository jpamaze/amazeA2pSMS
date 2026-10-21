-- KEYS[1] wallet hash
-- ARGV[1] top-up amount, ARGV[2] database balance after the top-up, ARGV[3] database credit limit
-- A wallet not yet in Redis is loaded from the database values (which already
-- include this top-up); otherwise the amount is added. Returns the new balance.
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'balance', ARGV[2], 'credit_limit', ARGV[3], 'held', 0)
  return tonumber(ARGV[2])
end
return redis.call('HINCRBY', KEYS[1], 'balance', ARGV[1])
