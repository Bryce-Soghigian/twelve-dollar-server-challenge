-- GET /posts/<random seeded id>
request = function()
  return wrk.format("GET", "/posts/" .. math.random(1, 500000))
end
