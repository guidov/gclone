for f in /home/guido/gclone/accounts/*.json; do python3 -c "import json; d=json.load(open('$f')); print(d['client_email'])"; done

