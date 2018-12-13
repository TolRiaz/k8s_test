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

/**
 * Controller for tooltips used for words that are too long for angular UI.
 * @final
 */
export default class MiddleEllipsisController {
  /**
   * Constructs middle ellipsis controller.
   * @ngInject
   * @param {!angular.JQLite} $element
   */
  constructor($element) {
    /** @private {!angular.JQLite} */
    this.element_ = $element;

    /** @export {string} Initialized from the scope. */
    this.displayString;
  }

  /**
   * Checks if text length is equal to surrounding container.
   * @returns {boolean}
   * @export
   */
  isTruncated() {
    let cOfsWidth = this.element_[0].querySelector('.kd-middleellipsis-displayStr').offsetWidth;
    return cOfsWidth > this.element_[0].offsetWidth;
  }
}
/**
 * Middle ellipsis component definition.
 * @type {!angular.Component}
 */
export const middleEllipsisComponent = {
  bindings: {
    'displayString': '@',
  },
  controller: MiddleEllipsisController,
  controllerAs: 'ellipsisCtrl',
  templateUrl: 'common/components/middleellipsis/middleellipsis.html',
};
