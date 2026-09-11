<?php
// Replays Number::formatMoney's arithmetic with the REAL currency and country
// rows, for the show_currency_code=false company. No entity is created and
// nothing is written: this only reads the static tables.
$pdo = new PDO('mysql:host=db;dbname='.getenv('DB_DATABASE'), getenv('DB_USERNAME'), getenv('DB_PASSWORD'));
$cur = $pdo->query("SELECT code,symbol,`precision`,thousand_separator,decimal_separator,swap_currency_symbol FROM currencies")->fetchAll(PDO::FETCH_ASSOC);
$byCode = [];
foreach ($cur as $c) { $byCode[$c['code']] = $c; }
$ctr = $pdo->query("SELECT iso_3166_2,thousand_separator,decimal_separator,swap_currency_symbol FROM countries")->fetchAll(PDO::FETCH_ASSOC);
$byIso = [];
foreach ($ctr as $c) { $byIso[$c['iso_3166_2']] = $c; }

function fmt($value, $code, $iso, $byCode, $byIso) {
    $currency = $byCode[$code];
    $thousand = $currency['thousand_separator'];
    $decimal  = $currency['decimal_separator'];
    $precision= (int)$currency['precision'];
    $swap     = (bool)$currency['swap_currency_symbol'];
    $country  = $byIso[$iso] ?? null;
    if (isset($country['thousand_separator']) && strlen($country['thousand_separator']) >= 1) $thousand = $country['thousand_separator'];
    if (isset($country['decimal_separator'])  && strlen($country['decimal_separator'])  >= 1) $decimal  = $country['decimal_separator'];
    if (isset($country['swap_currency_symbol']) && $country['swap_currency_symbol'] == 1)     $swap = true;
    $_value = $value;
    $out = number_format($value, $precision, $decimal, $thousand);
    $symbol = $currency['symbol'];
    if ($swap) return $out.' '.trim($symbol);          // show_currency_code = false
    if ($_value < 0) { $out = substr($out, 1); $symbol = "-{$symbol}"; }
    return $symbol.$out;
}

$cases = [];
foreach ([['EUR','NL'],['EUR','DE'],['EUR','IE'],['EUR','FR'],['EUR','MT'],['EUR','BE'],['EUR','EE'],['EUR','ES'],['EUR','IT'],['EUR','AT'],['EUR','LU'],['EUR','CY'],['EUR','LV'],['EUR','ZZ'],['USD','US'],['GBP','GB'],['CHF','CH'],['SEK','SE'],['PLN','PL'],['DKK','DK']] as [$c,$i]) {
    foreach ([9, 9.5, 1234.56, 1234567.89, -9, -1234.5, 0] as $v) {
        $cases[] = sprintf("%s|%s|%s|%s", $c, $i, number_format($v,2,'.',''), fmt($v,$c,$i,$byCode,$byIso));
    }
}
echo implode("\n", $cases), "\n";
