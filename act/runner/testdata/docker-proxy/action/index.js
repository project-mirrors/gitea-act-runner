const assert = require('node:assert/strict');
const {once} = require('node:events');
const fs = require('node:fs');
const http = require('node:http');

async function request(method, path, body) {
  const req = http.request({
    socketPath: '/var/run/docker.sock',
    method,
    path,
    headers: {'Content-Type': 'application/json'},
    signal: AbortSignal.timeout(10000),
  });
  req.end(JSON.stringify(body));
  const [res] = await once(req, 'response');
  req.on('error', (error) => res.destroy(error));
  let data = '';
  for await (const chunk of res.setEncoding('utf8')) {
    data += chunk;
  }
  assert(res.statusCode >= 200 && res.statusCode < 300, `${method} ${path}: ${res.statusCode} ${data}`);
  return data;
}

async function main() {
  assert(fs.statSync('/var/run/docker.sock').isSocket(), 'Docker mount must be a socket');
  assert.equal(await request('GET', '/_ping'), 'OK');
  const api = `/v${JSON.parse(await request('GET', '/version')).ApiVersion}`;
  const name = process.env.PROXY_TEST_RESOURCE;
  const job = process.env.JOB_CONTAINER_NAME;
  const post = process.env.STATE_post === 'true';
  assert(name);
  assert(job);
  if (!post) {
    await request('POST', `${api}/volumes/create`, {Name: name});
    await request('POST', `${api}/networks/create`, {Name: name});
    await request('POST', `${api}/networks/${name}/connect`, {Container: job});
    fs.appendFileSync(process.env.GITHUB_STATE, 'post=true\n');
  }
  const label = process.env.PROXY_TEST_MODE === 'proxy' ? job : undefined;
  assert.equal(JSON.parse(await request('GET', `${api}/volumes/${name}`)).Labels?.['com.gitea.runner.job'], label);
  const network = JSON.parse(await request('GET', `${api}/networks/${name}`));
  assert.equal(network.Labels?.['com.gitea.runner.job'], label);
  assert(Object.values(network.Containers).some((container) => container.Name === job));
  if (post) {
    console.log('docker proxy post verified');
  }
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
