#!/usr/bin/env python3
"""Optional, synthetic NewAPI -> actual CPA process -> fixture contract test.

No production credentials/configuration are consumed. The Go overlay does not
change either checkout and ordinary CI has no sibling repository dependency.
"""
import argparse
import datetime
import hashlib
import json
import os
import signal
from pathlib import Path
import subprocess
import tempfile


def run_bounded(command, cwd, log, timeout, env=None):
    process = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    try:
        return process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        return 124
    finally:
        # All members of this new group are this synthetic run's own tools and
        # fixture processes. Clean up even if a Go timeout skips test cleanups.
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()


def source_digest(root):
    paths = subprocess.check_output(['git', 'ls-files', '-co', '--exclude-standard'], cwd=root, text=True).splitlines()
    h = hashlib.sha256()
    for name in sorted(set(paths)):
        if name.endswith('.go') or name in ('go.mod', 'go.sum'):
            path = root / name
            if path.is_file():
                h.update(name.encode() + b'\0' + path.read_bytes() + b'\0')
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cpa-root', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    cpa_root = args.cpa_root.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    output = args.output.resolve()
    summary = {'timestamp_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
               'synthetic_only': True, 'newapi_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip(),
               'cpa_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=cpa_root, text=True).strip(),
               'newapi_source_sha256': source_digest(root), 'cpa_source_sha256': source_digest(cpa_root)}
    summary['runner_sha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    summary['fixture_sha256'] = hashlib.sha256((root / 'tools/fixtures/cpa_terminal_contract_test.go.tmpl').read_bytes()).hexdigest()
    with tempfile.TemporaryDirectory(prefix='newapi-cpa-contract-') as directory:
        temp = Path(directory)
        binary = temp / 'cpa'
        with (output / 'build.log').open('w') as log:
            build_code = run_bounded(['go', 'build', '-o', str(binary), './cmd/cpa'], cpa_root, log, 600)
        summary['build_exit_code'] = build_code
        if build_code == 0:
            summary['cpa_binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
            fixture = temp / 'cpa_terminal_contract_test.go'
            fixture.write_bytes((root / 'tools/fixtures/cpa_terminal_contract_test.go.tmpl').read_bytes())
            overlay = temp / 'overlay.json'
            overlay.write_text(json.dumps({'Replace': {str(root / 'relay/cpa_process_terminal_test.go'): str(fixture)}}))
            env = {k: v for k, v in os.environ.items() if not k.startswith(('CPA_', 'FIXTURE_'))}
            env.update({'CPA_CONTRACT_BINARY': str(binary), 'CPA_CONTRACT_OUTPUT': str(output), 'GIN_MODE': 'release'})
            with (output / 'test.log').open('w') as log:
                test_code = run_bounded(['go', 'test', '-overlay=' + str(overlay), './relay', '-run', '^TestCPAProcessTerminalContract$', '-count=1', '-timeout=90s', '-v'], root, log, 180, env)
            summary['test_exit_code'] = test_code
            cases = output / 'cases.json'
            if cases.exists(): summary['cases'] = json.loads(cases.read_text())
        summary['source_unchanged_during_run'] = (summary['cpa_source_sha256'] == source_digest(cpa_root) and summary['newapi_source_sha256'] == source_digest(root))
    summary['passed'] = summary.get('test_exit_code') == 0 and summary.get('build_exit_code') == 0 and summary['source_unchanged_during_run']
    (output / 'summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps({'passed': summary['passed'], 'cases': len(summary.get('cases', [])), 'summary': str(output / 'summary.json')}))
    raise SystemExit(0 if summary['passed'] else 1)


if __name__ == '__main__':
    main()
