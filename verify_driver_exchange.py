#!/usr/bin/env python3
"""Verify modernc -> native SQLite -> modernc on one real, synthetic database.

This runs host binaries; it does not establish Android app-domain compatibility.
"""
import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--out', type=Path, default=ROOT.parent / 'build/sqlite-driver-exchange')
args = parser.parse_args()
args.out.mkdir(parents=True, exist_ok=True)
out = args.out.resolve()
# Never overwrite a prior acceptance database.
run_dir = Path(tempfile.mkdtemp(prefix='run-', dir=out))
db = run_dir / 'shared.sqlite'
binaries = {}
for name, tags in [('modernc', []), ('native', ['-tags', 'onit_sqlite_native'])]:
    binary = out / ('adviser-' + name)
    subprocess.run(['go', 'build', *tags, '-o', str(binary), '.'], cwd=ROOT,
                   env={**os.environ, 'CGO_ENABLED': '1'}, check=True)
    binaries[name] = binary

token = subprocess.check_output([binaries['modernc'], 'issue', '-db', db], text=True).strip()
other_token = subprocess.check_output([binaries['modernc'], 'issue', '-db', db], text=True).strip()

@contextmanager
def service(name):
    with socket.socket() as address:
        address.bind(('127.0.0.1', 0))
        port = address.getsockname()[1]
    log = (run_dir / (name + '.log')).open('a')
    process = subprocess.Popen([binaries[name], 'serve', '-db', db, '-listen', f'127.0.0.1:{port}'],
                               stdout=subprocess.DEVNULL, stderr=log)
    def call(method, route, payload=None, credential=token):
        body = None if payload is None else json.dumps(payload, ensure_ascii=False).encode()
        request = urllib.request.Request(f'http://127.0.0.1:{port}' + route, data=body,
                    method=method, headers={'Authorization': 'Bearer ' + credential,
                                            'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)
    try:
        for _ in range(100):
            if process.poll() is not None:
                raise RuntimeError(f'{name} service exited; see its private log')
            try:
                call('GET', '/v1/portrait')
                break
            except urllib.error.URLError:
                time.sleep(.05)
        else:
            raise RuntimeError(f'{name} service did not become ready')
        yield call
    finally:
        process.terminate()
        process.wait(timeout=10)
        log.close()

material = {'kind': 'self', 'title': '迁移回归的虚构手记',
            'text': '我每周三练习大提琴。\n我喜欢桂花茶。'}
with service('modernc') as call:
    source = call('POST', '/v1/sources', material)
    before = call('GET', '/v1/portrait')
    practice, = [item for item in before if '大提琴' in item['text']]
    tea, = [item for item in before if '桂花茶' in item['text']]
    proposal = call('POST', '/v1/proposals', {'title': '练琴', 'basis': practice['id']})
    goal = call('POST', '/v1/proposals/' + proposal['id'] + '/accept',
                {'title': '练琴', 'step': '先练五分钟长弓', 'unit': '分钟',
                 'basis': {'id': practice['id'], 'version': practice['version']}})

correction = '我现在把大提琴练习留在周六上午。'
with service('native') as call:
    assert call('GET', '/v1/portrait') == before
    call('PATCH', '/v1/memories/' + practice['id'],
         {'text': correction, 'version': practice['version']})
    after = call('GET', '/v1/portrait')
    updated, = [item for item in after if item['id'] == practice['id']]
    assert updated['text'] == correction and updated['version'] == practice['version'] + 1
    assert tea in after
    assert call('GET', '/v1/portrait', credential=other_token) == []

with service('modernc') as call:
    assert call('GET', '/v1/portrait') == after
    saved_goals = call('GET', '/v1/goals')
    assert len(saved_goals) == 1 and saved_goals[0]['id'] == goal['id']
    assert saved_goals[0]['step'] == '先练五分钟长弓'
    assert saved_goals[0]['basis']['currentText'] == correction
    saved_source = call('GET', '/v1/sources/' + source['id'])
    assert saved_source['text'] == material['text']
    assert call('GET', '/v1/portrait', credential=other_token) == []

assert db.read_bytes().startswith(b'SQLite format 3\0')
assert db.stat().st_mode & 0o777 == 0o600
result = {'database': str(db), 'sequence': ['modernc: create source and goal',
          'native: reopen and correct one fact', 'modernc: reopen and verify correction'],
          'correction': correction, 'unchanged_fact': tea['text'],
          'chosen_action': saved_goals[0]['step'], 'source_text_preserved': True,
          'second_user_remains_empty': True, 'file_mode': '0600',
          'android_app_domain_verified': False}
(run_dir / 'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')
print(run_dir / 'result.json')
