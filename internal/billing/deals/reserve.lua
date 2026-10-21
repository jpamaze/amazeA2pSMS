-- KEYS[1] deal:{id} (field used)   KEYS[2] deal:{id}:res (msgID -> segments)
-- ARGV: msgID, segments, cap, used in PostgreSQL (seeds a missing counter), expire-at ms
-- Returns {1, remaining} if reserved (or already reserved for this message),
-- {0, remaining} if the cap would be exceeded.
local cap = tonumber(ARGV[3])
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 1 then
  return {1, cap - tonumber(redis.call('HGET', KEYS[1], 'used') or 0)}
end
local used = redis.call('HGET', KEYS[1], 'used')
if not used then
  used = tonumber(ARGV[4])
  redis.call('HSET', KEYS[1], 'used', used)
  redis.call('PEXPIREAT', KEYS[1], ARGV[5])
else
  used = tonumber(used)
end
local n = tonumber(ARGV[2])
if used + n > cap then
  return {0, cap - used}
end
redis.call('HINCRBY', KEYS[1], 'used', n)
redis.call('HSET', KEYS[2], ARGV[1], n)
redis.call('PEXPIREAT', KEYS[2], ARGV[5])
return {1, cap - used - n}
