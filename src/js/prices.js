/* Built-in list prices for pricing the report by instance type. Each cloud
 * lists on-demand Linux prices in USD per hour in one reference region, with
 * vCPUs and memory, and a price factor for other regions. Factors are close
 * to the general-purpose types' ratio and are approximate for other families.
 * Plain script: runs in the browser (SP.prices) and under node.
 *
 * The default pricing in model.js keeps its own AWS us-east-1 table, which
 * the Go summary shares; this catalog only prices what the reader picks.
 */
(function () {
  'use strict';

  // type: [vCPUs, GiB, USD per hour in the reference region]
  var CLOUDS = {
    aws: {
      name: 'AWS', ref: 'us-east-1',
      regions: {
        'us-east-1': 1, 'us-east-2': 1, 'us-west-2': 1, 'us-west-1': 1.167, 'ca-central-1': 1.115,
        'eu-west-1': 1.115, 'eu-west-2': 1.156, 'eu-central-1': 1.198, 'eu-north-1': 1.063,
        'ap-south-1': 1.052, 'ap-southeast-1': 1.25, 'ap-southeast-2': 1.25, 'ap-northeast-1': 1.292, 'sa-east-1': 1.594
      },
      types: {
        't3.medium': [2, 4, 0.0416], 't3.large': [2, 8, 0.0832], 't3.xlarge': [4, 16, 0.1664], 't3.2xlarge': [8, 32, 0.3328],
        't3a.medium': [2, 4, 0.0376], 't3a.large': [2, 8, 0.0752], 't3a.xlarge': [4, 16, 0.1504],
        'm5.large': [2, 8, 0.096], 'm5.xlarge': [4, 16, 0.192], 'm5.2xlarge': [8, 32, 0.384], 'm5.4xlarge': [16, 64, 0.768], 'm5.8xlarge': [32, 128, 1.536],
        'm6i.large': [2, 8, 0.096], 'm6i.xlarge': [4, 16, 0.192], 'm6i.2xlarge': [8, 32, 0.384], 'm6i.4xlarge': [16, 64, 0.768], 'm6i.8xlarge': [32, 128, 1.536],
        'm6a.large': [2, 8, 0.0864], 'm6a.xlarge': [4, 16, 0.1728], 'm6a.2xlarge': [8, 32, 0.3456], 'm6a.4xlarge': [16, 64, 0.6912],
        'm7i.large': [2, 8, 0.1008], 'm7i.xlarge': [4, 16, 0.2016], 'm7i.2xlarge': [8, 32, 0.4032], 'm7i.4xlarge': [16, 64, 0.8064],
        'm7g.large': [2, 8, 0.0816], 'm7g.xlarge': [4, 16, 0.1632], 'm7g.2xlarge': [8, 32, 0.3264], 'm7g.4xlarge': [16, 64, 0.6528],
        'c5.large': [2, 4, 0.085], 'c5.xlarge': [4, 8, 0.17], 'c5.2xlarge': [8, 16, 0.34], 'c5.4xlarge': [16, 32, 0.68], 'c5.9xlarge': [36, 72, 1.53],
        'c6i.large': [2, 4, 0.085], 'c6i.xlarge': [4, 8, 0.17], 'c6i.2xlarge': [8, 16, 0.34], 'c6i.4xlarge': [16, 32, 0.68],
        'c7i.large': [2, 4, 0.08925], 'c7i.xlarge': [4, 8, 0.1785], 'c7i.2xlarge': [8, 16, 0.357], 'c7i.4xlarge': [16, 32, 0.714],
        'r5.large': [2, 16, 0.126], 'r5.xlarge': [4, 32, 0.252], 'r5.2xlarge': [8, 64, 0.504], 'r5.4xlarge': [16, 128, 1.008], 'r5.8xlarge': [32, 256, 2.016],
        'r6i.large': [2, 16, 0.126], 'r6i.xlarge': [4, 32, 0.252], 'r6i.2xlarge': [8, 64, 0.504], 'r6i.4xlarge': [16, 128, 1.008],
        'r7i.large': [2, 16, 0.1323], 'r7i.xlarge': [4, 32, 0.2646], 'r7i.2xlarge': [8, 64, 0.5292], 'r7i.4xlarge': [16, 128, 1.0584]
      }
    },
    gcp: {
      name: 'Google Cloud', ref: 'us-central1',
      regions: {
        'us-central1': 1, 'us-east1': 1, 'us-west1': 1, 'us-east4': 1.126, 'us-west2': 1.201,
        'europe-west1': 1.1, 'europe-west4': 1.101, 'europe-west2': 1.288, 'europe-west3': 1.288,
        'asia-southeast1': 1.232, 'asia-northeast1': 1.283, 'australia-southeast1': 1.41, 'southamerica-east1': 1.588
      },
      types: {
        'e2-medium': [2, 4, 0.033503],
        'e2-standard-2': [2, 8, 0.067006], 'e2-standard-4': [4, 16, 0.134012], 'e2-standard-8': [8, 32, 0.268024], 'e2-standard-16': [16, 64, 0.536048], 'e2-standard-32': [32, 128, 1.072096],
        'e2-highmem-2': [2, 16, 0.090416], 'e2-highmem-4': [4, 32, 0.180832], 'e2-highmem-8': [8, 64, 0.361664], 'e2-highmem-16': [16, 128, 0.723328],
        'e2-highcpu-2': [2, 2, 0.049468], 'e2-highcpu-4': [4, 4, 0.098936], 'e2-highcpu-8': [8, 8, 0.197872], 'e2-highcpu-16': [16, 16, 0.395744],
        'n1-standard-1': [1, 3.75, 0.0475], 'n1-standard-2': [2, 7.5, 0.095], 'n1-standard-4': [4, 15, 0.19], 'n1-standard-8': [8, 30, 0.38], 'n1-standard-16': [16, 60, 0.76],
        'n2-standard-2': [2, 8, 0.097118], 'n2-standard-4': [4, 16, 0.194236], 'n2-standard-8': [8, 32, 0.388472], 'n2-standard-16': [16, 64, 0.776944], 'n2-standard-32': [32, 128, 1.553888],
        'n2-highmem-2': [2, 16, 0.131014], 'n2-highmem-4': [4, 32, 0.262028], 'n2-highmem-8': [8, 64, 0.524056], 'n2-highmem-16': [16, 128, 1.048112],
        'n2-highcpu-2': [2, 2, 0.071696], 'n2-highcpu-4': [4, 4, 0.143392], 'n2-highcpu-8': [8, 8, 0.286784], 'n2-highcpu-16': [16, 16, 0.573568],
        'n2d-standard-2': [2, 8, 0.084492], 'n2d-standard-4': [4, 16, 0.168984], 'n2d-standard-8': [8, 32, 0.337968], 'n2d-standard-16': [16, 64, 0.675936],
        'c2-standard-4': [4, 16, 0.2088], 'c2-standard-8': [8, 32, 0.4176], 'c2-standard-16': [16, 64, 0.8352]
      }
    },
    azure: {
      name: 'Azure', ref: 'eastus',
      regions: {
        'eastus': 1, 'eastus2': 1, 'westus2': 1, 'centralus': 1.125, 'westus': 1.198,
        'northeurope': 1.094, 'westeurope': 1.198, 'uksouth': 1.156,
        'southeastasia': 1.302, 'japaneast': 1.354, 'australiaeast': 1.354
      },
      types: {
        'Standard_B2s': [2, 4, 0.0416], 'Standard_B2ms': [2, 8, 0.0832], 'Standard_B4ms': [4, 16, 0.166], 'Standard_B8ms': [8, 32, 0.333],
        'Standard_DS2_v2': [2, 7, 0.146], 'Standard_DS3_v2': [4, 14, 0.293], 'Standard_DS4_v2': [8, 28, 0.585],
        'Standard_D2s_v3': [2, 8, 0.096], 'Standard_D4s_v3': [4, 16, 0.192], 'Standard_D8s_v3': [8, 32, 0.384], 'Standard_D16s_v3': [16, 64, 0.768],
        'Standard_D2s_v5': [2, 8, 0.096], 'Standard_D4s_v5': [4, 16, 0.192], 'Standard_D8s_v5': [8, 32, 0.384], 'Standard_D16s_v5': [16, 64, 0.768], 'Standard_D32s_v5': [32, 128, 1.536],
        'Standard_D2ds_v5': [2, 8, 0.113], 'Standard_D4ds_v5': [4, 16, 0.226], 'Standard_D8ds_v5': [8, 32, 0.452],
        'Standard_D2as_v5': [2, 8, 0.086], 'Standard_D4as_v5': [4, 16, 0.172], 'Standard_D8as_v5': [8, 32, 0.344], 'Standard_D16as_v5': [16, 64, 0.688],
        'Standard_E2s_v5': [2, 16, 0.126], 'Standard_E4s_v5': [4, 32, 0.252], 'Standard_E8s_v5': [8, 64, 0.504], 'Standard_E16s_v5': [16, 128, 1.008],
        'Standard_E2as_v5': [2, 16, 0.113], 'Standard_E4as_v5': [4, 32, 0.226], 'Standard_E8as_v5': [8, 64, 0.452],
        'Standard_F2s_v2': [2, 4, 0.0846], 'Standard_F4s_v2': [4, 8, 0.169], 'Standard_F8s_v2': [8, 16, 0.338], 'Standard_F16s_v2': [16, 32, 0.677]
      }
    }
  };

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
      var c = cloudOf(n.hostType);
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

  // Rates from the nodes' own types and regions, the way model.pricing prices
  // them but across every cloud here: total price over total allocatable.
  // Nodes in a region the catalog lacks are priced at the reference region.
  function fromNodes(nodes) {
    var cost = 0, cores = 0, gib = 0, priced = 0, unknownRegion = 0;
    (nodes || []).forEach(function (n) {
      var c = cloudOf(n.hostType);
      if (!c || !(n.cores > 0) || !(n.gib > 0)) return;
      var known = !!CLOUDS[c].regions[n.region];
      if (!known) unknownRegion++;
      cost += lookup(c, known ? n.region : CLOUDS[c].ref, n.hostType).hourly;
      cores += n.cores; gib += n.gib; priced++;
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
