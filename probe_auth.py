import urllib.request, json
url='https://apioauthcid.correos.es/Api/Authorize?redirect_uri=https%3A%2F%2Fexample.com%2Fcallback&response_type=code&state=abc&scope=openid&client_id=066a6ffb-c90c-4f3e-98ec-0f56cfa5643e'
data=json.dumps({'username':'demo@example.com','password':'demo'}).encode()
req=urllib.request.Request(url, data=data, headers={'Accept':'application/json','Content-Type':'application/json','ApplicationOid':'066a6ffb-c90c-4f3e-98ec-0f56cfa5643e','Accept-Language':'es-ES,es;q=0.9'}, method='POST')
try:
    resp=urllib.request.urlopen(req, timeout=20)
    print('STATUS', resp.status)
    print(resp.read().decode('utf-8','ignore'))
except Exception as e:
    print(type(e).__name__, e)
    if hasattr(e, 'read'):
        print(e.read().decode('utf-8','ignore'))
