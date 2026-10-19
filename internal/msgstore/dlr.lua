-- KEYS[1] msg hash, KEYS[2] set of counted vendor IDs
-- ARGV[1] vendor message ID, ARGV[2] delivered (1/0), ARGV[3] ttl seconds
-- Returns {counted, parts, parts_done, parts_ok}; counted = -1 when no record.
if redis.call('EXISTS', KEYS[1]) == 0 then
  return {-1, 0, 0, 0}
end
local added = redis.call('SADD', KEYS[2], ARGV[1])
if added == 1 then
  redis.call('EXPIRE', KEYS[2], ARGV[3])
  redis.call('HINCRBY', KEYS[1], 'parts_done', 1)
  if ARGV[2] == '1' then
    redis.call('HINCRBY', KEYS[1], 'parts_ok', 1)
  end
end
local v = redis.call('HMGET', KEYS[1], 'parts', 'parts_done', 'parts_ok')
return {added, tonumber(v[1] or '0'), tonumber(v[2] or '0'), tonumber(v[3] or '0')}
