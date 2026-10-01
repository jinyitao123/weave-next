#!/usr/bin/env python3
"""Bootstrap and verify a private single-host Compose deployment without logging credentials."""

import datetime
import json
import os
from pathlib import Path
import subprocess
import sys
import urllib.error
import urllib.request


def read_env(path):
    values = {}
    for line in path.read_text().splitlines():
        if not line.strip() or line.lstrip().startswith('#') or '=' not in line:
            continue
        key, value = line.split('=', 1)
        values[key.strip()] = value.strip().strip('\"\'')
    return values


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + '.next')
    temporary.write_text(json.dumps(value, indent=2) + '\n')
    os.chmod(temporary, 0o600)
    temporary.replace(path)


def get_json(url, api_key=None):
    headers = {'Authorization': 'Bearer ' + api_key} if api_key else {}
    with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=15) as response:
        return json.load(response)


def web_workbench_enabled(values):
    """The web Workbench is retired; it is deployed only when server.env opts in."""
    return values.get('WEAVE_WEB_WORKBENCH', '').strip().lower() in ('1', 'true', 'yes', 'on')


def prepare_workbench_storage(values):
    paths = []
    for key in ('WORKBENCH_DATA_PATH', 'WORKBENCH_WORKSPACE_PATH'):
        raw_path = values.get(key)
        if not raw_path:
            raise RuntimeError(key + ' must point to persistent Workbench storage.')
        path = Path(raw_path)
        if not path.is_absolute() or path.is_symlink():
            raise RuntimeError(key + ' must be an absolute, non-symlink path.')
        paths.append((key, path))

    resolved = [path.resolve() for _, path in paths]
    first, second = resolved
    if first == second or first in second.parents or second in first.parents:
        raise RuntimeError('Workbench data and workspace paths must be separate directories.')

    prepared = []
    for key, path in paths:
        if path.exists():
            if not path.is_dir():
                raise RuntimeError(key + ' exists but is not a directory.')
            state = 'preserved'
        else:
            path.mkdir(parents=True, mode=0o750)
            os.chmod(path, 0o750)
            state = 'created_empty'
        prepared.append({'setting': key, 'path': str(path), 'state': state})
    return prepared


def main():
    os.umask(0o077)
    action, env_name, state_name, *arguments = sys.argv[1:]
    env_path, state = Path(env_name), Path(state_name)
    values = read_env(env_path)
    local_api = 'http://127.0.0.1:' + values.get('WEAVE_API_PORT', '8080')
    if action == 'workbench-enabled':
        print('true' if web_workbench_enabled(values) else 'false')
    elif action == 'prepare-workbench-storage':
        prepared = prepare_workbench_storage(values) if web_workbench_enabled(values) else []
        print(json.dumps(prepared, indent=2))
    elif action == 'bootstrap':
        if not values.get('WEAVE_API_KEY'):
            assert arguments[0] == '--'
            output = subprocess.check_output(arguments[1:] + [
                'exec', '-T', 'weave', '/usr/local/bin/weave', 'bootstrap',
                '--api-url', 'http://weave:8080',
            ], text=True)
            api_key = json.loads(output).get('api_key')
            if not api_key:
                raise RuntimeError('Existing API key is missing from server.env; restore the saved credential.')
            lines = [line for line in env_path.read_text().splitlines()
                     if line.split('=', 1)[0].strip() != 'WEAVE_API_KEY']
            temporary = env_path.with_suffix('.env.next')
            temporary.write_text('\n'.join(lines + ['WEAVE_API_KEY=' + api_key]) + '\n')
            os.chmod(temporary, 0o600)
            temporary.replace(env_path)
            values['WEAVE_API_KEY'] = api_key
        get_json(local_api + '/v1/auth/me', values['WEAVE_API_KEY'])
        connection = {
            'api_url': values['WEAVE_RUNTIME_SERVER_URL'],
            'api_key': values['WEAVE_API_KEY'],
            'admin_username': values.get('WEAVE_ADMIN_USER', 'admin'),
            'admin_password': values['WEAVE_ADMIN_PASS'],
        }
        if web_workbench_enabled(values):
            connection['workbench_url'] = 'http://' + values['WORKBENCH_PUBLIC_AUTHORITY']
        write_json(state / 'connection.json', connection)
        print('Bootstrap and authenticated API verified; connection.json saved privately.')
    elif action == 'verify':
        expected_sha = arguments[0]
        health = get_json(local_api + '/v1/health')
        if health.get('build_commit') != expected_sha:
            raise RuntimeError('The running API does not match the requested commit.')
        if get_json(local_api + '/v1/ready').get('status') != 'ready':
            raise RuntimeError('API dependencies are not ready.')
        get_json(local_api + '/v1/runtimes', values['WEAVE_API_KEY'])
        status = None
        if web_workbench_enabled(values):
            url = 'http://127.0.0.1:' + values.get('WORKBENCH_PORT', '3080') + '/'
            request = urllib.request.Request(url, headers={'Host': values['WORKBENCH_PUBLIC_AUTHORITY']})
            try:
                with urllib.request.urlopen(request, timeout=15) as response:
                    status = response.status
            except urllib.error.HTTPError as error:
                status = error.code
            if status not in (200, 401):
                raise RuntimeError('Workbench gateway is not ready: HTTP ' + str(status))
        receipt = {'commit': expected_sha, 'branch': 'main', 'health': health,
                   'ready': True, 'authenticated_api': True, 'workbench_http_status': status,
                   'web_workbench': web_workbench_enabled(values),
                   'verified_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}
        write_json(state / 'last-success.json', receipt)
        print(json.dumps(receipt, indent=2))
    else:
        raise ValueError('Unknown deployment operation')


if __name__ == '__main__':
    main()
