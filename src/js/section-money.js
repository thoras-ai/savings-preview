/* Section 2: where the money is. Not built yet; it shows what it will cover. */
(function () {
  'use strict';

  SP.sections.register({
    id: 'money', number: 2, title: 'Where the money is', placeholder: true,
    render: function (host) {
      host.innerHTML = '<p class="placeholder">Spend and savings by namespace and workload.</p>';
    }
  });
})();
