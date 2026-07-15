import urllib.request
url='https://apioauthcid.correos.es/Api/UtilitiesCorreosId/GetUrlRedirectOauth?applicationOid=066a6ffb-c90c-4f3e-98ec-0f56cfa5643e'
req=urllib.request.Request(url, headers={'Accept':'application/json','Origin':'https://identidad.correos.es','Referer':'https://identidad.correos.es/','ApplicationOid':'066a6ffb-c90c-4f3e-98ec-0f56cfa5643e'})
try:
    with urllib.request.urlopen(req, timeout=20) as resp:
        print('STATUS', resp.status)
        print(resp.read().decode('utf-8','ignore'))
except Exception as e:
    print(type(e).__name__, e)
    if hasattr(e, 'read'):
        print(e.read().decode('utf-8','ignore'))
