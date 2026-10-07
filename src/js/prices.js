/* Prices for pricing the report by an instance type the reader picks. The
 * types and their on-demand Linux prices in USD per hour come from the
 * product's node price table that model.js prices the nodes with, so a type
 * picked in its cloud's reference region costs what the default pricing
 * charges for it. Each cloud also has a price factor for other regions; the
 * factors are close to the general-purpose types' ratio and are approximate
 * for other families. Plain script: runs in the browser (SP.prices) and under node.
 */
(function () {
  'use strict';

  var model = typeof module !== 'undefined' && module.exports ? require('./model.js') : SP.model;

  var CLOUDS = {
    aws: {
      name: 'AWS',
      regions: {
        'us-east-1': 1, 'us-east-2': 1, 'us-west-2': 1, 'us-west-1': 1.167, 'ca-central-1': 1.115,
        'eu-west-1': 1.115, 'eu-west-2': 1.156, 'eu-central-1': 1.198, 'eu-north-1': 1.063,
        'ap-south-1': 1.052, 'ap-southeast-1': 1.25, 'ap-southeast-2': 1.25, 'ap-northeast-1': 1.292, 'sa-east-1': 1.594
      }
    },
    gcp: {
      name: 'Google Cloud',
      regions: {
        'us-central1': 1, 'us-east1': 1, 'us-west1': 1, 'us-east4': 1.126, 'us-west2': 1.201,
        'europe-west1': 1.1, 'europe-west4': 1.101, 'europe-west2': 1.288, 'europe-west3': 1.288,
        'asia-southeast1': 1.232, 'asia-northeast1': 1.283, 'australia-southeast1': 1.41, 'southamerica-east1': 1.588
      }
    },
    azure: {
      name: 'Azure',
      regions: {
        'eastus': 1, 'eastus2': 1, 'westus2': 1, 'centralus': 1.125, 'westus': 1.198,
        'northeurope': 1.094, 'westeurope': 1.198, 'uksouth': 1.156,
        'southeastasia': 1.302, 'japaneast': 1.354, 'australiaeast': 1.354
      }
    }
  };

  // type: [vCPUs, GiB, on-demand USD per hour in the reference region]
  Object.keys(CLOUDS).forEach(function (c) {
    var table = model.priceTable[c], types = Object.create(null);
    Object.keys(table.types).forEach(function (t) {
      var row = table.types[t];
      types[t] = [row[0], row[1] / 1024, row[2]];
    });
    CLOUDS[c].ref = table.region;
    CLOUDS[c].types = types;
  });

  // The price of one instance type in one region, or null when either is unknown.
  function lookup(cloud, region, type) {
    var c = CLOUDS[cloud], spec = c && c.types[type], factor = c && c.regions[region];
    if (!spec || !factor) return null;
    var hourly = spec[2] * factor;
    return {
      cloud: cloud, region: region, type: type, cores: spec[0], gib: spec[1], hourly: hourly,
      perCore: hourly / spec[0], perGiB: hourly / spec[1], scaled: region !== c.ref
    };
  }

  // A pick is kept in the link as cloud/region/type.
  function key(p) { return p.cloud + '/' + p.region + '/' + p.type; }

  function parseKey(k) {
    var parts = String(k || '').split('/');
    return parts.length === 3 ? lookup(parts[0], parts[1], parts[2]) : null;
  }

  // The cloud an instance type belongs to, or null.
  function cloudOf(type) {
    for (var c in CLOUDS) if (CLOUDS[c].types[type]) return c;
    return null;
  }

  // The cloud and region most of the nodes are in, for preselecting the picker.
  function guess(nodes) {
    var counts = {}, best = null;
    (nodes || []).forEach(function (n) {
      var p = model.priceNode(n), c = p.reason ? cloudOf(n.hostType) : p.cloud;
      if (!c) return;
      var region = CLOUDS[c].regions[n.region] ? n.region : CLOUDS[c].ref;
      var k = c + '/' + region;
      counts[k] = (counts[k] || 0) + 1;
      if (!best || counts[k] > counts[best]) best = k;
    });
    if (!best) return null;
    var p = best.split('/');
    return { cloud: p[0], region: p[1] };
  }

  // Rates from the default pricing with each node's price scaled to its own
  // region. Nodes in a region the catalog lacks keep the reference price.
  function fromNodes(nodes) {
    var cost = 0, cores = 0, gib = 0, priced = 0, unknownRegion = 0;
    (nodes || []).forEach(function (n) {
      var p = model.priceNode(n);
      if (p.reason) return;
      var factor = CLOUDS[p.cloud].regions[n.region];
      if (!factor) unknownRegion++;
      cost += p.hourly * (factor || 1);
      cores += p.cores; gib += p.gib; priced++;
    });
    if (!priced) return null;
    return { perCore: cost / cores, perGiB: cost / gib, priced: priced, nodes: (nodes || []).length, unknownRegion: unknownRegion };
  }

  // m5 for m5.xlarge, n2-standard for n2-standard-4, Standard_D#s_v5 for Standard_D4s_v5.
  function family(type) {
    if (type.indexOf('.') > 0) return type.split('.')[0];
    if (/^Standard_/.test(type)) return type.replace(/\d+/, '#');
    return type.replace(/-\d+$/, '');
  }

  // Every type of a cloud, by family and then size.
  function types(cloud) {
    var t = CLOUDS[cloud].types;
    return Object.keys(t).sort(function (a, b) {
      var fa = family(a), fb = family(b);
      if (fa !== fb) return fa < fb ? -1 : 1;
      return t[a][0] - t[b][0] || t[a][1] - t[b][1];
    });
  }

  var api = { CLOUDS: CLOUDS, lookup: lookup, key: key, parseKey: parseKey, cloudOf: cloudOf, guess: guess, fromNodes: fromNodes, types: types };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else (self.SP = self.SP || {}).prices = api;
})();
