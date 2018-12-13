// Copyright 2017 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import {namespaceParam} from '../../chrome/state';
import {stateName as loginState} from '../../login/state';

import {DEFAULT_NAMESPACE, namespaceSelectComponent} from './component';
import {NamespaceService} from './service';

/**
 * Angular module global namespace selection components.
 */
export default angular
    .module(
        'kubernetesDashboard.common.namespace',
        [
          'ngMaterial',
          'ngResource',
          'ui.router',
        ])
    .component('kdNamespaceSelect', namespaceSelectComponent)
    .service('kdNamespaceService', NamespaceService)
    .run(ensureNamespaceParamPresent);

/**
 * Ensures that namespaceParam is present in the URL.
 * @param {!angular.$location} $location
 * @param {!kdUiRouter.$transitions} $transitions
 * @param {!kdUiRouter.$state} $state
 * @ngInject
 */
function ensureNamespaceParamPresent($location, $transitions, $state) {
  /**
   * Helper function which redirect namespace param when the given namespace
   * in the transition is undefined.
   * @returns {boolean|angular.$q.Promise}
   */
  function changeNamespaceParamIfNeeded() {
    const transition = $state.transition;

    if (!transition) {
      return true;
    }

    const namespace = transition.params()[namespaceParam];
    if (namespace === undefined && transition.to().name !== loginState) {
      return $state.target(transition.to(), {[namespaceParam]: DEFAULT_NAMESPACE});
    }
    return true;
  }

  $transitions.onEnter({}, () => {
    return changeNamespaceParamIfNeeded();
  });
}
